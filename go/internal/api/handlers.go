package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"serverhub/internal/auth"
	"serverhub/internal/config"
	"serverhub/internal/debug"
	"serverhub/internal/local"
	sshclient "serverhub/internal/ssh"
)

type serverInfo struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Host   string               `json:"host"`
	Port   int                  `json:"port"`
	User   string               `json:"user"`
	Tags   []string             `json:"tags"`
	Notes  string               `json:"notes"`
	Online bool                 `json:"online"`
	QA     []config.QuickAction `json:"quick_actions"`
}

type Hub struct {
	cfg        *config.Config
	cfgPath    string
	hue        int
	shade      int
	animations bool
	mu         sync.RWMutex // protects entries map and cfg.Servers slice
	entries    map[string]*sshclient.ServerEntry
	local      *local.Collector
	sse        *sseHub
	notify     chan struct{}
	guard      *Guard
	upgrader   websocket.Upgrader
	auth       *auth.Manager
}

func NewHub(cfg *config.Config, cfgPath string, entries []*sshclient.ServerEntry, lc *local.Collector) *Hub {
	m := make(map[string]*sshclient.ServerEntry, len(entries))
	for _, e := range entries {
		m[e.ID] = e
	}
	anim := cfg.Animations == nil || *cfg.Animations // default true
	h := &Hub{
		cfg:        cfg,
		cfgPath:    cfgPath,
		entries:    m,
		hue:        cfg.Hue,
		shade:      cfg.Shade,
		animations: anim,
		local:      lc,
		sse:        newSSEHub(),
		notify:     make(chan struct{}, 1),
		guard:      NewGuard(cfg.Server.Bind, cfg.Server.WebPort),
	}
	// Fail the upgrade with a proper HTTP error status when a check fails.
	h.upgrader = websocket.Upgrader{
		CheckOrigin: h.checkOrigin,
		Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
			http.Error(w, "websocket upgrade rejected", status)
		},
	}
	// Wire immediate SSE pushes on any stat change
	for _, e := range entries {
		e.OnUpdate = h.triggerBroadcast
	}
	lc.SetOnUpdate(h.triggerBroadcast)
	go h.runBroadcaster()
	return h
}

// SetAuthManager wires the auth manager in (used by the onboarding handler).
// Must be called before RegisterRoutes.
func (h *Hub) SetAuthManager(m *auth.Manager) { h.auth = m }

// ThemeValues returns the current accent hue and shade (thread-safe). Used as
// the auth manager's Theme provider for the public status endpoint.
func (h *Hub) ThemeValues() (int, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hue, h.shade
}

// RegisterRoutes registers all hub routes on mux.
func (h *Hub) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/ping", h.handlePing)
	mux.HandleFunc("/api/servers", h.handleServers)
	mux.HandleFunc("/api/server/", h.routeServer)
	mux.HandleFunc("/api/local/terminal", h.handleLocalTerminal)
	mux.HandleFunc("/api/local/events", h.handleLocalEvents)
	mux.HandleFunc("/api/local/stats", h.handleLocalStats)
	mux.HandleFunc("/api/local/containers", h.handleLocalContainers)
	mux.HandleFunc("/api/local/container/", h.handleLocalContainer)
	mux.HandleFunc("/api/local/action/", h.handleLocalAction)
	mux.HandleFunc("/api/local/reboot", h.handleLocalReboot)
	mux.HandleFunc("/api/local/shutdown", h.handleLocalShutdown)
	mux.HandleFunc("/api/local", h.handleLocalInfo)
	mux.HandleFunc("/api/dashboard/events", h.handleDashboardEvents)
	mux.HandleFunc("/api/audit", h.handleAudit)
	mux.HandleFunc("/api/appconfig", h.handleAppConfig)
	mux.HandleFunc("/api/terminal/config/", h.handleTerminalConfig)
	mux.HandleFunc("/api/onboarding", h.handleOnboarding)
	mux.HandleFunc("/api/debug/enable", h.handleDebugEnable)
	mux.HandleFunc("/api/debug/disable", h.handleDebugDisable)
	mux.HandleFunc("/api/debug/status", h.handleDebugStatus)
	mux.HandleFunc("/api/debug/stream", h.handleDebugStream)
}

func (h *Hub) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"app": "serverhub"})
}

// ── /api/servers ─────────────────────────────────────────────────────────────

func (h *Hub) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.mu.RLock()
		defer h.mu.RUnlock()
		var list []serverInfo
		for _, s := range h.cfg.Servers {
			entry := h.entries[s.ID]
			online := false
			if entry != nil {
				online = entry.GetStats().Online
			}
			list = append(list, toInfo(s, online))
		}
		writeJSON(w, list)

	case http.MethodPost:
		h.handleAddServer(w, r)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var host config.Host
	if err := json.NewDecoder(r.Body).Decode(&host); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Generate ID from name if not provided
	if host.ID == "" {
		host.ID = config.Slugify(host.Name)
	}
	if host.Port == 0 {
		host.Port = 22
	}
	if host.Tags == nil {
		host.Tags = []string{}
	}
	if host.QuickActions == nil {
		host.QuickActions = []config.QuickAction{}
	}

	// Validate
	if host.ID == "" || host.Host == "" || host.User == "" {
		http.Error(w, "id, host, and user are required", http.StatusBadRequest)
		return
	}
	// IDs are woven into HTML/URLs by the UI — enforce the strict charset
	// (also blocks the reserved __local__ sentinel).
	if err := config.ValidateID(host.ID); err != nil {
		http.Error(w, "invalid server id: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	// Check duplicate ID
	for _, s := range h.cfg.Servers {
		if s.ID == host.ID {
			h.mu.Unlock()
			http.Error(w, fmt.Sprintf("server id %q already exists", host.ID), http.StatusConflict)
			return
		}
	}

	// Expand ~ in key path
	host.Key = config.ExpandHomePath(host.Key)

	h.cfg.Servers = append(h.cfg.Servers, host)
	entry := sshclient.NewServerEntry(host.ID, host.Host, host.Port, host.User, host.Key)
	entry.OnUpdate = h.triggerBroadcast
	h.entries[host.ID] = entry

	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		// Non-fatal: in-memory state is updated; warn but continue
		fmt.Printf("warning: could not save config: %v\n", err)
	}
	h.mu.Unlock()

	sshclient.StartOne(entry)
	LogAction(host.ID, "server added")
	writeJSON(w, toInfo(host, false))
	h.triggerBroadcast()
}

// ── /api/server/:id ───────────────────────────────────────────────────────────

func (h *Hub) routeServer(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/server/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	// Server-level operations: PUT (update) and DELETE
	if len(parts) == 1 || parts[1] == "" {
		switch r.Method {
		case http.MethodPut:
			h.handleUpdateServer(w, r, id)
		case http.MethodDelete:
			h.handleDeleteServer(w, r, id)
		default:
			http.NotFound(w, r)
		}
		return
	}

	rest := parts[1]

	h.mu.RLock()
	entry, ok := h.entries[id]
	var serverCfg *config.Host
	for i := range h.cfg.Servers {
		if h.cfg.Servers[i].ID == id {
			s := h.cfg.Servers[i]
			serverCfg = &s
			break
		}
	}
	h.mu.RUnlock()

	if !ok || serverCfg == nil {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	switch {
	case rest == "stats" && r.Method == http.MethodGet:
		writeJSON(w, entry.GetStats())

	case rest == "containers" && r.Method == http.MethodGet:
		writeJSON(w, entry.GetContainers())

	case strings.HasPrefix(rest, "container/"):
		h.handleContainer(w, r, entry, serverCfg, strings.TrimPrefix(rest, "container/"))

	case rest == "reboot" && r.Method == http.MethodPost:
		h.handleReboot(w, serverCfg)

	case rest == "shutdown" && r.Method == http.MethodPost:
		h.handleShutdown(w, serverCfg)

	case strings.HasPrefix(rest, "action/"):
		h.handleAction(w, r, serverCfg, strings.TrimPrefix(rest, "action/"))

	case rest == "quick-actions" && r.Method == http.MethodPatch:
		h.handleServerQuickActions(w, r, id)

	case rest == "terminal":
		h.handleTerminal(w, r, serverCfg)

	case rest == "events" && r.Method == http.MethodGet:
		h.handleServerEvents(w, r, entry)

	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) handleUpdateServer(w http.ResponseWriter, r *http.Request, id string) {
	var updated config.Host
	if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	updated.ID = id // ID is immutable
	if updated.Port == 0 {
		updated.Port = 22
	}
	if updated.Tags == nil {
		updated.Tags = []string{}
	}
	if updated.QuickActions == nil {
		updated.QuickActions = []config.QuickAction{}
	}
	updated.Key = config.ExpandHomePath(updated.Key)

	h.mu.Lock()
	idx := -1
	for i, s := range h.cfg.Servers {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		h.mu.Unlock()
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	// Terminal colours are managed by /api/terminal/config/<id>; the server
	// edit form doesn't carry them, so keep the existing override.
	updated.Terminal = h.cfg.Servers[idx].Terminal
	// Preserve last-run metadata for quick actions that survived the edit.
	mergeQuickActionState(h.cfg.Servers[idx].QuickActions, updated.QuickActions)

	// Stop old polling goroutine and start fresh
	if old, ok := h.entries[id]; ok {
		old.Stop()
	}
	h.cfg.Servers[idx] = updated
	entry := sshclient.NewServerEntry(updated.ID, updated.Host, updated.Port, updated.User, updated.Key)
	entry.OnUpdate = h.triggerBroadcast
	h.entries[id] = entry

	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		fmt.Printf("warning: could not save config: %v\n", err)
	}
	h.mu.Unlock()

	sshclient.StartOne(entry)
	LogAction(id, "server updated")
	writeJSON(w, toInfo(updated, false))
	h.triggerBroadcast()
}

func (h *Hub) handleDeleteServer(w http.ResponseWriter, r *http.Request, id string) {
	h.mu.Lock()
	idx := -1
	for i, s := range h.cfg.Servers {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		h.mu.Unlock()
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}

	if entry, ok := h.entries[id]; ok {
		entry.Stop()
		delete(h.entries, id)
	}
	h.cfg.Servers = append(h.cfg.Servers[:idx], h.cfg.Servers[idx+1:]...)

	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		fmt.Printf("warning: could not save config: %v\n", err)
	}
	h.mu.Unlock()

	LogAction(id, "server deleted")
	writeJSON(w, map[string]bool{"ok": true})
	h.triggerBroadcast()
}

// ── Sub-handlers ──────────────────────────────────────────────────────────────

func (h *Hub) handleContainer(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host, path string) {
	// Method check first, before any work is done.
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	action := parts[1]

	var cmd string
	switch action {
	case "start":
		cmd = fmt.Sprintf("docker start %s", shellQuote(name))
	case "stop":
		cmd = fmt.Sprintf("docker stop %s", shellQuote(name))
	case "restart":
		cmd = fmt.Sprintf("docker restart %s", shellQuote(name))
	default:
		http.NotFound(w, r)
		return
	}

	out, err := sshclient.RunAction(s.Host, s.Port, s.User, s.Key, cmd)
	LogAction(s.ID, fmt.Sprintf("container %s %s", action, name))
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	go func() { entry.GetContainers() }()
	writeJSON(w, map[string]interface{}{"ok": true, "output": out})
}

func cleanQuickActions(in []config.QuickAction) []config.QuickAction {
	out := make([]config.QuickAction, 0, len(in))
	for _, a := range in {
		label := strings.TrimSpace(a.Label)
		command := strings.TrimSpace(a.Command)
		if label == "" || command == "" {
			continue
		}
		out = append(out, config.QuickAction{Label: label, Command: command})
	}
	return out
}

// mergeQuickActionState carries last-run metadata forward when an action is
// edited but keeps the same label and command.
func mergeQuickActionState(old, next []config.QuickAction) {
	for i := range next {
		for _, prev := range old {
			if prev.Label == next[i].Label && prev.Command == next[i].Command {
				next[i].LastRan = prev.LastRan
				next[i].LastStatus = prev.LastStatus
				break
			}
		}
	}
}

// handleServerQuickActions updates just the quick-action list for a server,
// without touching any of its other fields.
func (h *Hub) handleServerQuickActions(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		QuickActions []config.QuickAction `json:"quick_actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	idx := -1
	for i := range h.cfg.Servers {
		if h.cfg.Servers[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		h.mu.Unlock()
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	qa := cleanQuickActions(body.QuickActions)
	mergeQuickActionState(h.cfg.Servers[idx].QuickActions, qa)
	h.cfg.Servers[idx].QuickActions = qa
	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		fmt.Printf("warning: could not save config: %v\n", err)
	}
	updated := h.cfg.Servers[idx]
	online := false
	if entry := h.entries[id]; entry != nil {
		online = entry.GetStats().Online
	}
	h.mu.Unlock()

	writeJSON(w, toInfo(updated, online))
}

func (h *Hub) handleReboot(w http.ResponseWriter, s *config.Host) {
	out, err := sshclient.RunAction(s.Host, s.Port, s.User, s.Key, "sudo reboot")
	LogAction(s.ID, "reboot")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleShutdown(w http.ResponseWriter, s *config.Host) {
	out, err := sshclient.RunAction(s.Host, s.Port, s.User, s.Key, "sudo shutdown now")
	LogAction(s.ID, "shutdown")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleAction(w http.ResponseWriter, r *http.Request, s *config.Host, idxStr string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(s.QuickActions) {
		http.Error(w, "invalid action index", http.StatusBadRequest)
		return
	}
	qa := s.QuickActions[idx]
	LogAction(s.ID, fmt.Sprintf("quick_action: %s", qa.Label))

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := sshclient.RunActionContext(ctx, s.Host, s.Port, s.User, s.Key, qa.Command)
	status := "success"
	if err != nil {
		status = "error"
		if r.Context().Err() != nil {
			status = "stopped"
		}
	}
	h.recordQuickActionRun(s.ID, idx, qa.Label, qa.Command, status)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Quick-Action-Status", status)
	w.WriteHeader(http.StatusOK)
	if err != nil {
		fmt.Fprintf(w, "%s\n\nError: %s\n", out, err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

// recordQuickActionRun stores the last-run time and outcome for a quick
// action. The config may have been edited while the command was running, so
// the action is matched by label+command rather than blindly by index.
func (h *Hub) recordQuickActionRun(target string, idx int, label, command, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var list *[]config.QuickAction
	if target == "__local__" {
		list = &h.cfg.Local.QuickActions
	} else {
		for i := range h.cfg.Servers {
			if h.cfg.Servers[i].ID == target {
				list = &h.cfg.Servers[i].QuickActions
				break
			}
		}
	}
	if list == nil {
		return
	}

	found := -1
	if idx >= 0 && idx < len(*list) && (*list)[idx].Label == label && (*list)[idx].Command == command {
		found = idx
	} else {
		for i := range *list {
			if (*list)[i].Label == label && (*list)[i].Command == command {
				found = i
				break
			}
		}
	}
	if found == -1 {
		return
	}

	(*list)[found].LastRan = time.Now().Unix()
	(*list)[found].LastStatus = status
	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		fmt.Printf("warning: could not save config: %v\n", err)
	}
}

// upgrade upgrades to a WebSocket with the strict origin check applied
// (belt-and-braces alongside the auth cookie middleware — cross-site
// handshakes fail with 403 before any shell is ever touched).
func (h *Hub) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return h.upgrader.Upgrade(w, r, nil)
}

func (h *Hub) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser client (no Origin header). The auth middleware still
		// gates the handshake on a valid session cookie.
		return true
	}
	return h.guard.originAllowed(origin)
}

// terminalSlots caps concurrent interactive shell sessions (local + remote).
const maxTerminalSessions = 8

var terminalSlots = make(chan struct{}, maxTerminalSessions)

// acquireTerminalSlot takes one of the finite terminal slots; ok is false
// when every slot is in use.
func acquireTerminalSlot() bool {
	select {
	case terminalSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Hub) handleTerminal(w http.ResponseWriter, r *http.Request, s *config.Host) {
	if !acquireTerminalSlot() {
		http.Error(w, "terminal session limit reached", http.StatusTooManyRequests)
		return
	}
	defer func() { <-terminalSlots }()
	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	sshclient.ProxyTerminal(ws, s.Host, s.Port, s.User, s.Key)
}

func (h *Hub) handleLocalTerminal(w http.ResponseWriter, r *http.Request) {
	if !acquireTerminalSlot() {
		http.Error(w, "terminal session limit reached", http.StatusTooManyRequests)
		return
	}
	defer func() { <-terminalSlots }()
	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	local.ProxyLocalTerminal(ws)
}

// ── /api/local ────────────────────────────────────────────────────────────────

type localInfoResponse struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Hostname     string               `json:"hostname"`
	Online       bool                 `json:"online"`
	Tags         []string             `json:"tags"`
	Notes        string               `json:"notes"`
	QuickActions []config.QuickAction `json:"quick_actions"`
}

func localInfo(hostname string, local config.LocalConfig) localInfoResponse {
	name := local.Name
	if name == "" {
		name = hostname
	}
	tags := local.Tags
	if tags == nil {
		tags = []string{}
	}
	qa := local.QuickActions
	if qa == nil {
		qa = []config.QuickAction{}
	}
	return localInfoResponse{
		ID:           "__local__",
		Name:         name,
		Hostname:     hostname,
		Online:       true,
		Tags:         tags,
		Notes:        local.Notes,
		QuickActions: qa,
	}
}

func (h *Hub) handleLocalInfo(w http.ResponseWriter, r *http.Request) {
	hostname := h.local.Hostname()
	switch r.Method {
	case http.MethodGet:
		h.mu.RLock()
		local := h.cfg.Local
		h.mu.RUnlock()
		writeJSON(w, localInfo(hostname, local))
	case http.MethodPatch:
		var body struct {
			Name         *string               `json:"name"`
			Tags         *[]string             `json:"tags"`
			Notes        *string               `json:"notes"`
			QuickActions *[]config.QuickAction `json:"quick_actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" {
				h.mu.Unlock()
				http.Error(w, "name cannot be empty", http.StatusBadRequest)
				return
			}
			h.cfg.Local.Name = name
		}
		if body.Tags != nil {
			tags := *body.Tags
			if tags == nil {
				tags = []string{}
			}
			h.cfg.Local.Tags = tags
		}
		if body.Notes != nil {
			h.cfg.Local.Notes = *body.Notes
		}
		if body.QuickActions != nil {
			qa := cleanQuickActions(*body.QuickActions)
			mergeQuickActionState(h.cfg.Local.QuickActions, qa)
			h.cfg.Local.QuickActions = qa
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.mu.Unlock()
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		local := h.cfg.Local
		h.mu.Unlock()
		writeJSON(w, localInfo(hostname, local))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /api/local actions / containers ───────────────────────────────────────────

func (h *Hub) handleLocalAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/local/action/"))
	h.mu.RLock()
	qa := h.cfg.Local.QuickActions
	h.mu.RUnlock()
	if err != nil || idx < 0 || idx >= len(qa) {
		http.Error(w, "invalid action index", http.StatusBadRequest)
		return
	}
	action := qa[idx]
	LogAction("__local__", fmt.Sprintf("quick_action: %s", action.Label))

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := local.RunShellContext(ctx, action.Command)
	status := "success"
	if err != nil {
		status = "error"
		if r.Context().Err() != nil {
			status = "stopped"
		}
	}
	h.recordQuickActionRun("__local__", idx, action.Label, action.Command, status)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Quick-Action-Status", status)
	w.WriteHeader(http.StatusOK)
	if err != nil {
		fmt.Fprintf(w, "%s\n\nError: %s\n", out, err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", out)
}

func (h *Hub) handleLocalContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	containers, err := local.CollectContainers()
	if err != nil {
		http.Error(w, "docker unavailable", http.StatusInternalServerError)
		return
	}
	if containers == nil {
		containers = []sshclient.Container{}
	}
	writeJSON(w, sshclient.ContainerList{
		UpdatedAt:  time.Now().Unix(),
		Containers: containers,
	})
}

func (h *Hub) handleLocalContainer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/local/container/"), "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	name, action := parts[0], parts[1]
	out, err := local.RunContainerAction(name, action)
	LogAction("__local__", fmt.Sprintf("container %s %s", action, name))
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "output": out})
}

func (h *Hub) handleLocalReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out, err := local.RunShell("sudo reboot")
	LogAction("__local__", "reboot")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleLocalShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out, err := local.RunShell("sudo shutdown now")
	LogAction("__local__", "shutdown")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleLocalStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, h.local.GetStats())
}

func (h *Hub) handleAudit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, GetAuditLog())
}

// ── /api/appconfig ────────────────────────────────────────────────────────────

func (h *Hub) handleAppConfig(w http.ResponseWriter, r *http.Request) {
	type appConfigResponse struct {
		Hue        int                   `json:"hue"`
		Shade      int                   `json:"shade"`
		Animations bool                  `json:"animations"`
		Terminal   config.TerminalConfig `json:"terminal"`
		Owner      string                `json:"owner"`
	}

	if r.Method == http.MethodGet {
		h.mu.RLock()
		resp := appConfigResponse{
			Hue:        h.hue,
			Shade:      h.shade,
			Animations: h.animations,
			Terminal:   h.cfg.Terminal,
			Owner:      h.cfg.Owner,
		}
		h.mu.RUnlock()
		writeJSON(w, resp)
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			Hue        *int  `json:"hue"`
			Shade      *int  `json:"shade"`
			Animations *bool `json:"animations"`
			Terminal   *struct {
				Background *string `json:"background"`
				Foreground *string `json:"foreground"`
				Accent     *string `json:"accent"`
			} `json:"terminal"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Terminal != nil {
			for _, c := range []*string{body.Terminal.Background, body.Terminal.Foreground, body.Terminal.Accent} {
				if c != nil && !config.ValidHexColor(*c) {
					http.Error(w, "terminal colors must be #rgb or #rrggbb hex values", http.StatusBadRequest)
					return
				}
			}
		}
		h.mu.Lock()
		if body.Hue != nil && *body.Hue >= 0 && *body.Hue <= 360 {
			h.hue = *body.Hue
			h.cfg.Hue = *body.Hue
		}
		if body.Shade != nil && *body.Shade >= 0 && *body.Shade <= 100 {
			h.shade = *body.Shade
			h.cfg.Shade = *body.Shade
		}
		if body.Animations != nil {
			h.animations = *body.Animations
			h.cfg.Animations = body.Animations
		}
		if body.Terminal != nil {
			if body.Terminal.Background != nil {
				h.cfg.Terminal.Background = *body.Terminal.Background
			}
			if body.Terminal.Foreground != nil {
				h.cfg.Terminal.Foreground = *body.Terminal.Foreground
			}
			if body.Terminal.Accent != nil {
				h.cfg.Terminal.Accent = *body.Terminal.Accent
			}
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			fmt.Printf("warning: could not save config: %v\n", err)
		}
		resp := appConfigResponse{
			Hue:        h.hue,
			Shade:      h.shade,
			Animations: h.animations,
			Terminal:   h.cfg.Terminal,
			Owner:      h.cfg.Owner,
		}
		h.mu.Unlock()
		writeJSON(w, resp)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// ── /api/terminal/config/<target> ─────────────────────────────────────────────
//
// Per-machine web-terminal colours. <target> is "__local__" or a server ID.
// Empty override fields inherit the global terminal colours (Settings → Web
// Terminal). GET returns both the effective colours and the override (null
// when the machine simply uses the global values).

type terminalConfigResponse struct {
	Effective config.TerminalConfig  `json:"effective"`
	Override  *config.TerminalConfig `json:"override"`
}

func mergeTerminalConfig(global config.TerminalConfig, override *config.TerminalConfig) config.TerminalConfig {
	eff := global
	if override != nil {
		if override.Background != "" {
			eff.Background = override.Background
		}
		if override.Foreground != "" {
			eff.Foreground = override.Foreground
		}
		if override.Accent != "" {
			eff.Accent = override.Accent
		}
	}
	return eff
}

func (h *Hub) handleTerminalConfig(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/api/terminal/config/")
	if target == "" || strings.Contains(target, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	var override **config.TerminalConfig
	if target == "__local__" {
		override = &h.cfg.LocalTerminal
	} else {
		for i := range h.cfg.Servers {
			if h.cfg.Servers[i].ID == target {
				override = &h.cfg.Servers[i].Terminal
				break
			}
		}
		if override == nil {
			http.Error(w, "unknown terminal target", http.StatusNotFound)
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, terminalConfigResponse{
			Effective: mergeTerminalConfig(h.cfg.Terminal, *override),
			Override:  *override,
		})

	case http.MethodPatch:
		var body struct {
			Background *string `json:"background"`
			Foreground *string `json:"foreground"`
			Accent     *string `json:"accent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, c := range []*string{body.Background, body.Foreground, body.Accent} {
			if c != nil && *c != "" && !config.ValidHexColor(*c) {
				http.Error(w, "terminal colors must be #rgb or #rrggbb hex values", http.StatusBadRequest)
				return
			}
		}
		if *override == nil {
			*override = &config.TerminalConfig{}
		}
		if body.Background != nil {
			(*override).Background = *body.Background
		}
		if body.Foreground != nil {
			(*override).Foreground = *body.Foreground
		}
		if body.Accent != nil {
			(*override).Accent = *body.Accent
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			fmt.Printf("warning: could not save config: %v\n", err)
		}
		writeJSON(w, terminalConfigResponse{
			Effective: mergeTerminalConfig(h.cfg.Terminal, *override),
			Override:  *override,
		})

	case http.MethodDelete:
		*override = nil
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			fmt.Printf("warning: could not save config: %v\n", err)
		}
		writeJSON(w, terminalConfigResponse{
			Effective: mergeTerminalConfig(h.cfg.Terminal, nil),
			Override:  nil,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── Onboarding (first-run wizard) ─────────────────────────────────────────────
//
// Completed exactly once: while no auth state exists, and only from loopback
// (the same trust model as /api/auth/setup). The owner's choices:
//
//   - user_name  → the user's name (greeting; config owner)
//   - password   → enables password auth (bcrypt) + session cookie
//   - opt_out    → records "no password" in auth.json; the Host/Origin/
//     preflight guards still protect the API
//   - hue/shade  → accent colour + surface shade (0=black … 100=white); nil skips
//
// The local machine card keeps its hostname-derived label; it can be
// renamed later via PATCH /api/local.
type onboardingBody struct {
	UserName string                 `json:"user_name"`
	Password string                 `json:"password"`
	OptOut   bool                   `json:"opt_out"`
	Hue      *int                   `json:"hue"`
	Shade    *int                   `json:"shade"`
	Terminal *config.TerminalConfig `json:"terminal"` // defaults derived from hue/shade
}

func (h *Hub) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.auth == nil {
		http.Error(w, "auth unavailable", http.StatusInternalServerError)
		return
	}
	if h.auth.OnboardingComplete() {
		http.Error(w, "onboarding already completed", http.StatusForbidden)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "setup must be completed on the host running ServerHub — open it locally", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body onboardingBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.OptOut && body.Password != "" {
		http.Error(w, "provide either a password or opt_out, not both", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.UserName)
	if len(name) > 64 {
		http.Error(w, "user_name must be 64 characters or fewer", http.StatusBadRequest)
		return
	}
	if body.Hue != nil && (*body.Hue < 0 || *body.Hue > 360) {
		http.Error(w, "hue must be 0-360", http.StatusBadRequest)
		return
	}
	if body.Shade != nil && (*body.Shade < 0 || *body.Shade > 100) {
		http.Error(w, "shade must be 0-100", http.StatusBadRequest)
		return
	}
	if body.Terminal != nil {
		for _, c := range []string{body.Terminal.Background, body.Terminal.Foreground, body.Terminal.Accent} {
			if c != "" && !config.ValidHexColor(c) {
				http.Error(w, "terminal colors must be #rgb or #rrggbb hex values", http.StatusBadRequest)
				return
			}
		}
	}

	if body.OptOut {
		if err := h.auth.OptOut(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	} else {
		if err := h.auth.SetupPassword(body.Password); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, auth.ErrAlreadySetup) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	}

	// Persist the user's choices (name always optional; blank = no greeting).
	changed := false
	h.mu.Lock()
	if name != "" {
		h.cfg.Owner = name
		changed = true
	}
	if body.Hue != nil {
		h.cfg.Hue = *body.Hue
		h.hue = *body.Hue // runtime mirror — both places, see handleTheme pattern
		changed = true
	}
	if body.Shade != nil {
		h.cfg.Shade = *body.Shade
		h.shade = *body.Shade
		changed = true
	}
	if body.Terminal != nil {
		if body.Terminal.Background != "" {
			h.cfg.Terminal.Background = body.Terminal.Background
		}
		if body.Terminal.Foreground != "" {
			h.cfg.Terminal.Foreground = body.Terminal.Foreground
		}
		if body.Terminal.Accent != "" {
			h.cfg.Terminal.Accent = body.Terminal.Accent
		}
		changed = true
	}
	if changed {
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			fmt.Printf("warning: could not save config during onboarding: %v\n", err)
		}
	}
	h.mu.Unlock()

	if body.OptOut {
		LogAction("auth", "onboarding completed — password declined")
	} else {
		LogAction("auth", "onboarding completed — password set")
	}
	h.triggerBroadcast()

	// Password mode: log the user straight in. Opt-out mode needs no session.
	if !body.OptOut {
		token, err := h.auth.CreateSession()
		if err != nil {
			http.Error(w, "could not create session", http.StatusInternalServerError)
			return
		}
		c := auth.NewSessionCookie(token)
		c.Secure = r.TLS != nil
		http.SetCookie(w, &c)
	}
	writeJSON(w, map[string]bool{"ok": true, "password_set": !body.OptOut})
}

// ── Debug ─────────────────────────────────────────────────────────────────────

func (h *Hub) handleDebugEnable(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "debug is a local-only feature", http.StatusForbidden)
		return
	}
	if debug.Global.Enable() {
		go h.updateDebugCounts()
	}
	writeJSON(w, map[string]bool{"enabled": true})
}

func (h *Hub) handleDebugDisable(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "debug is a local-only feature", http.StatusForbidden)
		return
	}
	debug.Global.Disable()
	writeJSON(w, map[string]bool{"enabled": false})
}

func (h *Hub) handleDebugStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		http.Error(w, "debug is a local-only feature", http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]bool{"enabled": debug.Global.Enabled()})
}

func (h *Hub) handleDebugStream(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		http.Error(w, "debug is a local-only feature", http.StatusForbidden)
		return
	}
	if !debug.Global.Enabled() {
		http.Error(w, "debug not enabled", http.StatusServiceUnavailable)
		return
	}
	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	// The debug stream carries snapshot/frame JSON — small writes only.
	ws.SetReadLimit(debugReadLimit)

	// Send full snapshot to newly connected client
	snap := debug.Global.GetSnapshot()
	if data, err := json.Marshal(snap); err == nil {
		if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}

	ch := debug.Global.Subscribe()
	defer debug.Global.Unsubscribe(ch)

	// Pump incoming messages to detect WebSocket close (keepalive: the client
	// is a browser that auto-pongs the control pings).
	sshclient.WSKeepalive(ws)

	connClosed := make(chan struct{})
	go func() {
		defer close(connClosed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-connClosed:
			return
		case frame, ok := <-ch:
			if !ok {
				return // collector was disabled
			}
			data, err := json.Marshal(frame)
			if err != nil {
				continue
			}
			if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}
}

// updateDebugCounts keeps the debug collector's server/online counts fresh.
// It exits automatically when debug is disabled.
func (h *Hub) updateDebugCounts() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		<-t.C
		if !debug.Global.Enabled() {
			return
		}
		h.mu.RLock()
		total := len(h.cfg.Servers)
		online := 0
		for _, e := range h.entries {
			if e.GetStats().Online {
				online++
			}
		}
		h.mu.RUnlock()
		debug.Global.SetServerCounts(total, online)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func toInfo(s config.Host, online bool) serverInfo {
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	qa := s.QuickActions
	if qa == nil {
		qa = []config.QuickAction{}
	}
	return serverInfo{
		ID:     s.ID,
		Name:   s.Name,
		Host:   s.Host,
		Port:   s.Port,
		User:   s.User,
		Tags:   tags,
		Notes:  s.Notes,
		Online: online,
		QA:     qa,
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// debugReadLimit caps frames on the debug stream (control frames only).
const debugReadLimit = 1 << 10

// requireMethod writes 405 and returns false when the request method differs.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// isLoopbackRequest reports whether the request originated from this machine.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
