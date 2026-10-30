package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"serverhub/internal/auth"
	"serverhub/internal/config"
	"serverhub/internal/debug"
	"serverhub/internal/jobs"
	"serverhub/internal/local"
	"serverhub/internal/media"
	"serverhub/internal/probe"
	"serverhub/internal/scheduler"
	sshclient "serverhub/internal/ssh"
)

type serverInfo struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Host     string               `json:"host"`
	Port     int                  `json:"port"`
	User     string               `json:"user"`
	Key      string               `json:"key"`
	Tags     []string             `json:"tags"`
	Notes    string               `json:"notes"`
	Online   bool                 `json:"online"`
	Favorite bool                 `json:"favorite"`
	QA       []config.QuickAction `json:"quick_actions"`
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
	jobs       *jobs.Registry
	sched      *scheduler.Scheduler
	localJobs  *local.JobRunner

	logMu      sync.Mutex
	logStreams map[string]int // active container-log streams per server
}

func NewHub(cfg *config.Config, cfgPath string, entries []*sshclient.ServerEntry, lc *local.Collector, jobReg *jobs.Registry) *Hub {
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
		jobs:       jobReg,
		logStreams: make(map[string]int),
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
	if runner, err := local.NewJobRunner(); err == nil {
		h.localJobs = runner
	}
	go h.runBroadcaster()
	return h
}

// SetAuthManager wires the auth manager in (used by the onboarding handler).
// Must be called before RegisterRoutes.
func (h *Hub) SetAuthManager(m *auth.Manager) { h.auth = m }

// SetScheduler wires the scheduler in (used by main and the schedule page).
func (h *Hub) SetScheduler(s *scheduler.Scheduler) { h.sched = s }

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
	mux.HandleFunc("/api/apps", h.handleApps)
	mux.HandleFunc("/api/apps/", h.handleApp)
	mux.HandleFunc("/api/media", h.handleMediaList)
	mux.HandleFunc("/api/media/fetch", h.handleMediaFetch)
	mux.HandleFunc("/api/media/", h.handleMediaItem)
	mux.HandleFunc("/media/", h.handleMedia)
	mux.HandleFunc("/api/background", h.handleBackground)
	mux.HandleFunc("/api/server/", h.routeServer)
	mux.HandleFunc("/api/local/terminal", h.handleLocalTerminal)
	mux.HandleFunc("/api/local/events", h.handleLocalEvents)
	mux.HandleFunc("/api/local/stats", h.handleLocalStats)
	mux.HandleFunc("/api/local/containers", h.handleLocalContainers)
	mux.HandleFunc("/api/local/images", h.handleLocalImages)
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
	mux.HandleFunc("/api/schedules", h.handleSchedules)
	mux.HandleFunc("/api/schedules/occurrences", h.handleScheduleOccurrences)
	mux.HandleFunc("/api/schedules/preview", h.handleSchedulePreview)
	mux.HandleFunc("/api/schedules/", h.handleSchedule)
	mux.HandleFunc("/api/scheduler/status", h.handleSchedulerStatus)
}

// handleSchedulerStatus exposes non-fatal problems found while loading
// schedules.conf (invalid entries are skipped at load, not fatal).
func (h *Hub) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.sched == nil {
		writeJSON(w, map[string][]string{"errors": {}})
		return
	}
	writeJSON(w, map[string][]string{"errors": h.sched.ConfigErrors()})
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

// ── /api/apps ────────────────────────────────────────────────────────────────

func (h *Hub) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.mu.RLock()
		apps := append([]config.App{}, h.cfg.Apps...)
		h.mu.RUnlock()
		sort.SliceStable(apps, func(i, j int) bool {
			if apps[i].Favorite != apps[j].Favorite {
				return apps[i].Favorite
			}
			return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
		})
		writeJSON(w, apps)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var body struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			Logo      string `json:"logo"`
			LogoMedia string `json:"logo_media"`
			Favorite  bool   `json:"favorite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		name, rawURL, logoSrc, err := normalizeApp(body.Name, body.URL, body.Logo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		logoFile := ""
		if body.LogoMedia != "" {
			if !media.Exists(body.LogoMedia) {
				http.Error(w, "library image not found", http.StatusBadRequest)
				return
			}
			logoFile, err = media.Copy(body.LogoMedia, name)
			if err != nil {
				http.Error(w, "could not copy logo: "+err.Error(), http.StatusBadRequest)
				return
			}
			logoSrc = media.ReadMeta(body.LogoMedia).SourceURL
		} else if logoSrc != "" {
			logoFile, err = media.Download(r.Context(), logoSrc, name, false)
			if err != nil {
				http.Error(w, "could not download logo: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		h.mu.Lock()
		id := h.uniqueAppIDLocked(config.Slugify(name))
		app := config.App{
			ID:         id,
			Name:       name,
			URL:        rawURL,
			Logo:       logoFile,
			LogoSource: logoSrc,
			Favorite:   body.Favorite,
		}
		previous := h.cfg.Apps
		h.cfg.Apps = append(h.cfg.Apps, app)
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.Apps = previous
			h.mu.Unlock()
			removeMediaFile(logoFile)
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		h.mu.Unlock()
		LogAction("apps", "app created: "+id)
		writeJSON(w, app)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handleApp(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/")
	parts := strings.SplitN(path, "/", 2)
	id := strings.TrimSpace(parts[0])
	action := ""
	if len(parts) == 2 {
		action = strings.Trim(parts[1], "/")
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if action != "" {
		if action == "logo/refresh" && r.Method == http.MethodPost {
			h.refreshAppLogo(w, r, id)
			return
		}
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var body struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			Logo      string `json:"logo"`
			LogoMedia string `json:"logo_media"`
			Favorite  *bool  `json:"favorite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		name, rawURL, logoSrc, err := normalizeApp(body.Name, body.URL, body.Logo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		h.mu.RLock()
		idx := h.appIndexLocked(id)
		var existing config.App
		if idx != -1 {
			existing = h.cfg.Apps[idx]
		}
		h.mu.RUnlock()
		if idx == -1 {
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}

		logoFile := existing.Logo
		if logoSrc != existing.LogoSource {
			if logoSrc == "" {
				logoFile = ""
			} else {
				logoFile, err = media.Download(r.Context(), logoSrc, name, false)
				if err != nil {
					http.Error(w, "could not download logo: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
		}

		h.mu.Lock()
		idx = h.appIndexLocked(id)
		if idx == -1 {
			h.mu.Unlock()
			removeMediaFile(logoFile)
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}
		previous := h.cfg.Apps[idx]
		h.cfg.Apps[idx].Name = name
		h.cfg.Apps[idx].URL = rawURL
		h.cfg.Apps[idx].Logo = logoFile
		h.cfg.Apps[idx].LogoSource = logoSrc
		if body.Favorite != nil {
			h.cfg.Apps[idx].Favorite = *body.Favorite
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.Apps[idx] = previous
			h.mu.Unlock()
			if logoFile != previous.Logo {
				removeMediaFile(logoFile)
			}
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		app := h.cfg.Apps[idx]
		h.mu.Unlock()

		if previous.Logo != "" && previous.Logo != logoFile {
			removeMediaFile(previous.Logo)
		}
		LogAction("apps", "app updated: "+id)
		writeJSON(w, app)

	case http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var body struct {
			Favorite *bool `json:"favorite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Favorite == nil {
			http.Error(w, "favorite is required", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		idx := h.appIndexLocked(id)
		if idx == -1 {
			h.mu.Unlock()
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}
		previous := h.cfg.Apps[idx].Favorite
		h.cfg.Apps[idx].Favorite = *body.Favorite
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.Apps[idx].Favorite = previous
			h.mu.Unlock()
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		app := h.cfg.Apps[idx]
		h.mu.Unlock()
		LogAction("apps", "app favorite updated: "+id)
		writeJSON(w, app)

	case http.MethodDelete:
		h.mu.Lock()
		idx := h.appIndexLocked(id)
		if idx == -1 {
			h.mu.Unlock()
			http.Error(w, "app not found", http.StatusNotFound)
			return
		}
		previous := h.cfg.Apps
		removed := h.cfg.Apps[idx]
		h.cfg.Apps = append(append([]config.App(nil), h.cfg.Apps[:idx]...), h.cfg.Apps[idx+1:]...)
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.Apps = previous
			h.mu.Unlock()
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		h.mu.Unlock()
		removeMediaFile(removed.Logo)
		LogAction("apps", "app deleted: "+id)
		writeJSON(w, map[string]bool{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) refreshAppLogo(w http.ResponseWriter, r *http.Request, id string) {
	h.mu.RLock()
	idx := h.appIndexLocked(id)
	var app config.App
	if idx != -1 {
		app = h.cfg.Apps[idx]
	}
	h.mu.RUnlock()
	if idx == -1 {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	source := app.LogoSource
	if source == "" && strings.HasPrefix(app.Logo, "http") {
		source = app.Logo
	}
	if source == "" {
		http.Error(w, "this app has no logo source", http.StatusBadRequest)
		return
	}
	file, err := media.Download(r.Context(), source, app.Name, false)
	if err != nil {
		http.Error(w, "could not refresh logo: "+err.Error(), http.StatusBadGateway)
		return
	}

	h.mu.Lock()
	idx = h.appIndexLocked(id)
	if idx == -1 {
		h.mu.Unlock()
		removeMediaFile(file)
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	old := h.cfg.Apps[idx].Logo
	h.cfg.Apps[idx].Logo = file
	h.cfg.Apps[idx].LogoSource = source
	if err := config.Save(h.cfgPath, h.cfg); err != nil {
		h.cfg.Apps[idx].Logo = old
		h.mu.Unlock()
		removeMediaFile(file)
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}
	app = h.cfg.Apps[idx]
	h.mu.Unlock()
	if old != "" && old != file {
		removeMediaFile(old)
	}
	LogAction("apps", "app logo refreshed: "+id)
	writeJSON(w, app)
}

func removeMediaFile(name string) {
	if name == "" || strings.Contains(name, "://") {
		return
	}
	_ = media.Remove(name)
}

func (h *Hub) appIndexLocked(id string) int {
	for i := range h.cfg.Apps {
		if h.cfg.Apps[i].ID == id {
			return i
		}
	}
	return -1
}

func (h *Hub) uniqueAppIDLocked(base string) string {
	if config.ValidateID(base) != nil {
		base = "app"
	}
	id := base
	for n := 2; h.appIndexLocked(id) != -1; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

func normalizeApp(name, rawURL, logo string) (string, string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", "", fmt.Errorf("name is required")
	}
	if len(name) > 80 {
		return "", "", "", fmt.Errorf("name must be 80 characters or fewer")
	}
	rawURL, err := normalizeHTTPURL(rawURL, true)
	if err != nil {
		return "", "", "", err
	}
	logo, err = normalizeHTTPURL(logo, false)
	if err != nil {
		return "", "", "", fmt.Errorf("logo: %v", err)
	}
	return name, rawURL, logo, nil
}

func normalizeHTTPURL(raw string, required bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return "", fmt.Errorf("URL is required")
		}
		return "", nil
	}
	if len(raw) > 2048 {
		return "", fmt.Errorf("URL is too long")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("URL must be http:// or https://")
	}
	return raw, nil
}

// ── /media and background ────────────────────────────────────────────────────

type mediaItem struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	SourceURL string `json:"source_url,omitempty"`
	Size      int64  `json:"size,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

func makeMediaItem(name string) mediaItem {
	meta := media.ReadMeta(name)
	kind := meta.Kind
	if kind == "" {
		kind = media.KindForName(name)
	}
	item := mediaItem{
		Name:      name,
		URL:       "/media/" + neturl.PathEscape(name),
		Kind:      kind,
		SourceURL: meta.SourceURL,
	}
	if path, err := media.Path(name); err == nil {
		if info, err := os.Stat(path); err == nil {
			item.Size = info.Size()
			item.UpdatedAt = info.ModTime().Unix()
		}
	}
	return item
}

func (h *Hub) handleMediaList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		names, err := media.List("")
		if err != nil {
			http.Error(w, "could not list media", http.StatusInternalServerError)
			return
		}
		kind := strings.TrimSpace(r.URL.Query().Get("kind"))
		items := make([]mediaItem, 0, len(names))
		for _, name := range names {
			item := makeMediaItem(name)
			if kind != "" && kind != "all" && item.Kind != kind {
				continue
			}
			items = append(items, item)
		}
		writeJSON(w, items)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, (12<<20)+(1<<20))
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			http.Error(w, "could not read upload", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "image is required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (12<<20)+1))
		if err != nil || len(data) == 0 || len(data) > 12<<20 {
			http.Error(w, "image must be between 1 byte and 12 MB", http.StatusBadRequest)
			return
		}
		kind := strings.TrimSpace(r.FormValue("kind"))
		prefix := "image"
		switch kind {
		case "background":
			prefix = "background"
		case "doc":
			prefix = "doc"
		default:
			kind = "image"
		}
		name, err := media.Save(prefix, data, header.Filename, kind == "background")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = media.WriteMeta(name, media.Meta{Kind: kind, CreatedAt: time.Now().Unix()})
		LogAction("media", "uploaded: "+name)
		writeJSON(w, makeMediaItem(name))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handleMediaFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL  string `json:"url"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	rawURL, err := normalizeHTTPURL(body.URL, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(body.Kind)
	prefix := "image"
	switch kind {
	case "background":
		prefix = "background"
	case "doc":
		prefix = "doc"
	default:
		kind = "image"
	}
	name, err := media.Download(r.Context(), rawURL, prefix, kind == "background")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Download writes a sidecar for the source URL; make the kind explicit.
	meta := media.ReadMeta(name)
	meta.Kind = kind
	_ = media.WriteMeta(name, meta)
	LogAction("media", "fetched: "+name)
	writeJSON(w, makeMediaItem(name))
}

func (h *Hub) handleMediaItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/media/")
	parts := strings.SplitN(path, "/", 2)
	name := strings.TrimSpace(parts[0])
	action := ""
	if len(parts) == 2 {
		action = strings.Trim(parts[1], "/")
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	if _, err := media.Path(name); err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case action == "refresh" && r.Method == http.MethodPost:
		meta := media.ReadMeta(name)
		source := meta.SourceURL
		kind := meta.Kind
		if kind == "" {
			kind = media.KindForName(name)
		}

		h.mu.RLock()
		var app config.App
		appIdx := -1
		for i := range h.cfg.Apps {
			if h.cfg.Apps[i].Logo == name {
				app = h.cfg.Apps[i]
				appIdx = i
				break
			}
		}
		h.mu.RUnlock()
		if source == "" && appIdx != -1 {
			source = app.LogoSource
		}
		if source == "" {
			http.Error(w, "this image has no source URL", http.StatusBadRequest)
			return
		}
		prefix := kind
		if appIdx != -1 {
			prefix = app.Name
			kind = "app"
		}
		newName, err := media.Download(r.Context(), source, prefix, kind == "background")
		if err != nil {
			http.Error(w, "could not refresh image: "+err.Error(), http.StatusBadGateway)
			return
		}
		newMeta := media.ReadMeta(newName)
		newMeta.Kind = kind
		_ = media.WriteMeta(newName, newMeta)

		h.mu.Lock()
		changed := false
		if h.cfg.BackgroundImage == name {
			h.cfg.BackgroundImage = newName
			changed = true
		}
		for i := range h.cfg.Apps {
			if h.cfg.Apps[i].Logo == name {
				h.cfg.Apps[i].Logo = newName
				if h.cfg.Apps[i].LogoSource == "" {
					h.cfg.Apps[i].LogoSource = source
				}
				changed = true
			}
		}
		if changed {
			if err := config.Save(h.cfgPath, h.cfg); err != nil {
				h.mu.Unlock()
				removeMediaFile(newName)
				http.Error(w, "could not save config", http.StatusInternalServerError)
				return
			}
		}
		h.mu.Unlock()
		removeMediaFile(name)
		LogAction("media", "refreshed: "+newName)
		writeJSON(w, makeMediaItem(newName))

	case action == "" && r.Method == http.MethodDelete:
		h.mu.Lock()
		changed := false
		if h.cfg.BackgroundImage == name {
			h.cfg.BackgroundImage = ""
			changed = true
		}
		for i := range h.cfg.Apps {
			if h.cfg.Apps[i].Logo == name {
				h.cfg.Apps[i].Logo = ""
				h.cfg.Apps[i].LogoSource = ""
				changed = true
			}
		}
		if changed {
			if err := config.Save(h.cfgPath, h.cfg); err != nil {
				h.mu.Unlock()
				http.Error(w, "could not save config", http.StatusInternalServerError)
				return
			}
		}
		h.mu.Unlock()
		removeMediaFile(name)
		LogAction("media", "deleted: "+name)
		writeJSON(w, map[string]bool{"ok": true})

	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	path, err := media.Path(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", media.ContentType(name))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Cache-Control", "no-store")
	}
	if strings.EqualFold(strings.TrimSpace(name[strings.LastIndex(name, ".")+1:]), "svg") {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	}
	http.ServeFile(w, r, path)
}

func (h *Hub) handleBackground(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.mu.RLock()
		name := h.cfg.BackgroundImage
		h.mu.RUnlock()
		path, err := media.Path(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, statErr := os.Stat(path); statErr != nil {
			// Migrate a background saved before media/ existed.
			if data, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), "..", filepath.Base(name))); readErr == nil {
				if migrated, saveErr := media.Save("background", data, name, true); saveErr == nil {
					_ = media.WriteMeta(migrated, media.Meta{Kind: "background", CreatedAt: time.Now().Unix()})
					path, _ = media.Path(migrated)
					name = migrated
					h.mu.Lock()
					h.cfg.BackgroundImage = migrated
					_ = config.Save(h.cfgPath, h.cfg)
					h.mu.Unlock()
				}
			}
			if _, statErr = os.Stat(path); statErr != nil {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", media.ContentType(name))
		if strings.EqualFold(filepath.Ext(name), ".svg") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
		}
		http.ServeFile(w, r, path)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, (12<<20)+(1<<20))
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			http.Error(w, "could not read upload", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "image is required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (12<<20)+1))
		if err != nil {
			http.Error(w, "could not read image", http.StatusBadRequest)
			return
		}
		if len(data) == 0 || len(data) > 12<<20 {
			http.Error(w, "image must be between 1 byte and 12 MB", http.StatusBadRequest)
			return
		}
		name, err := media.Save("background", data, header.Filename, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = media.WriteMeta(name, media.Meta{Kind: "background", CreatedAt: time.Now().Unix()})

		h.mu.Lock()
		previous := h.cfg.BackgroundImage
		h.cfg.BackgroundImage = name
		if h.cfg.BackgroundBlur == 0 {
			h.cfg.BackgroundBlur = 12
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.BackgroundImage = previous
			h.mu.Unlock()
			removeMediaFile(name)
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		blur := h.cfg.BackgroundBlur
		h.mu.Unlock()

		LogAction("settings", "background updated")
		writeJSON(w, map[string]interface{}{"ok": true, "background_image": name, "background_blur": blur})

	case http.MethodDelete:
		h.mu.Lock()
		previous := h.cfg.BackgroundImage
		if previous == "" {
			h.mu.Unlock()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
		h.cfg.BackgroundImage = ""
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.BackgroundImage = previous
			h.mu.Unlock()
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		h.mu.Unlock()
		removeMediaFile(previous)
		LogAction("settings", "background removed")
		writeJSON(w, map[string]bool{"ok": true})

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
	host.QuickActions = cleanQuickActions(host.QuickActions)

	// Validate
	if host.ID == "" || host.Host == "" || host.User == "" {
		http.Error(w, "id, host, and user are required", http.StatusBadRequest)
		return
	}
	if !validPort(host.Port) {
		http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
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

	if err := config.SaveAll(h.cfgPath, h.cfg); err != nil {
		// Roll back so memory and disk stay consistent.
		h.cfg.Servers = h.cfg.Servers[:len(h.cfg.Servers)-1]
		delete(h.entries, host.ID)
		h.mu.Unlock()
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
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

	if id == "__local__" {
		h.mu.RLock()
		runner := h.localJobs
		h.mu.RUnlock()
		if runner == nil {
			http.Error(w, "local jobs require tmux", http.StatusServiceUnavailable)
			return
		}
		switch {
		case rest == "jobs" && r.Method == http.MethodGet:
			h.handleListJobs(w, r, runner, id)
		case rest == "jobs" && r.Method == http.MethodPost:
			h.handleStartJob(w, r, runner, id)
		case strings.HasPrefix(rest, "jobs/"):
			h.handleJobRoute(w, r, runner, id, strings.TrimPrefix(rest, "jobs/"))
		default:
			http.NotFound(w, r)
		}
		return
	}

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

	case rest == "info" && r.Method == http.MethodGet:
		writeJSON(w, entry.GetInfo())

	case rest == "favorite" && r.Method == http.MethodPatch:
		h.handleFavoriteServer(w, r, id)

	case rest == "containers" && r.Method == http.MethodGet:
		writeJSON(w, entry.GetContainers())

	case rest == "images" && r.Method == http.MethodGet:
		writeJSON(w, entry.GetImages())

	case rest == "probe" && r.Method == http.MethodPost:
		h.handleProbe(w, r, entry, serverCfg)

	case rest == "images/refresh" && r.Method == http.MethodPost:
		ctx, cancel := context.WithTimeout(r.Context(), imageRefreshTimeout)
		defer cancel()
		list, err := entry.RefreshImages(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		LogAction(id, "images refreshed")
		writeJSON(w, list)

	case strings.HasPrefix(rest, "container/") && strings.HasSuffix(rest, "/logs") && r.Method == http.MethodGet:
		name := strings.TrimSuffix(strings.TrimPrefix(rest, "container/"), "/logs")
		h.handleContainerLogs(w, r, entry, id, name)

	case strings.HasPrefix(rest, "container/"):
		h.handleContainer(w, r, entry, serverCfg, strings.TrimPrefix(rest, "container/"))

	case rest == "reboot" && r.Method == http.MethodPost:
		h.handleReboot(w, r, entry, serverCfg)

	case rest == "shutdown" && r.Method == http.MethodPost:
		h.handleShutdown(w, r, entry, serverCfg)

	case strings.HasPrefix(rest, "action/"):
		h.handleAction(w, r, entry, serverCfg, strings.TrimPrefix(rest, "action/"))

	case rest == "quick-actions" && r.Method == http.MethodPatch:
		h.handleServerQuickActions(w, r, id)

	case rest == "terminal":
		h.handleTerminal(w, r, serverCfg)

	case rest == "events" && r.Method == http.MethodGet:
		h.handleServerEvents(w, r, entry)

	case rest == "jobs" && r.Method == http.MethodGet:
		h.handleListJobs(w, r, entry, id)

	case rest == "jobs" && r.Method == http.MethodPost:
		h.handleStartJob(w, r, entry, id)

	case strings.HasPrefix(rest, "jobs/"):
		h.handleJobRoute(w, r, entry, id, strings.TrimPrefix(rest, "jobs/"))

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
	if !validPort(updated.Port) {
		http.Error(w, "port must be between 1 and 65535", http.StatusBadRequest)
		return
	}
	if updated.Host == "" || updated.User == "" {
		http.Error(w, "host and user are required", http.StatusBadRequest)
		return
	}
	updated.Tags = append([]string{}, updated.Tags...)
	updated.QuickActions = cleanQuickActions(updated.QuickActions)
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

	// Terminal colours are managed by /api/terminal/config/<id> and favorites
	// by the star endpoint; the server edit form doesn't carry them.
	updated.Terminal = h.cfg.Servers[idx].Terminal
	updated.Favorite = h.cfg.Servers[idx].Favorite
	// Preserve last-run metadata for quick actions that survived the edit.
	mergeQuickActionState(h.cfg.Servers[idx].QuickActions, updated.QuickActions)

	oldHost := h.cfg.Servers[idx]
	oldEntry := h.entries[id]
	h.cfg.Servers[idx] = updated
	entry := sshclient.NewServerEntry(updated.ID, updated.Host, updated.Port, updated.User, updated.Key)
	entry.OnUpdate = h.triggerBroadcast
	h.entries[id] = entry

	if err := config.SaveAll(h.cfgPath, h.cfg); err != nil {
		h.cfg.Servers[idx] = oldHost
		if oldEntry != nil {
			h.entries[id] = oldEntry
		} else {
			delete(h.entries, id)
		}
		h.mu.Unlock()
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}
	h.mu.Unlock()

	// The old entry keeps polling until the new config is safely on disk.
	if oldEntry != nil {
		oldEntry.Stop()
	}
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

	entry := h.entries[id]
	removed := h.cfg.Servers[idx]
	h.cfg.Servers = append(h.cfg.Servers[:idx], h.cfg.Servers[idx+1:]...)
	delete(h.entries, id)

	if err := config.SaveAll(h.cfgPath, h.cfg); err != nil {
		// Restore the in-memory state; the polling goroutine was left running.
		h.cfg.Servers = append(h.cfg.Servers, config.Host{})
		copy(h.cfg.Servers[idx+1:], h.cfg.Servers[idx:])
		h.cfg.Servers[idx] = removed
		if entry != nil {
			h.entries[id] = entry
		}
		h.mu.Unlock()
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}
	h.mu.Unlock()

	if entry != nil {
		entry.Stop()
	}
	if h.jobs != nil {
		h.jobs.ForgetServer(id)
	}
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
	if !sshclient.ValidContainerName(name) {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}

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

	ctx, cancel := context.WithTimeout(r.Context(), containerActionTimeout)
	defer cancel()
	out, err := entry.RunCommand(ctx, cmd)
	LogAction(s.ID, fmt.Sprintf("container %s %s", action, name))
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	entry.RefreshContainers()
	entry.RefreshImagesAsync()
	writeJSON(w, map[string]interface{}{"ok": true, "output": out})
}

// ── Container logs ───────────────────────────────────────────────────────────

func logTail(r *http.Request) int {
	tail := defaultContainerLogTail
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	if tail < 1 {
		tail = 1
	}
	if tail > 10000 {
		tail = 10000
	}
	return tail
}

func (h *Hub) handleContainerLogs(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, serverID, name string) {
	if !sshclient.ValidContainerName(name) {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	tail := logTail(r)
	script := fmt.Sprintf("exec 2>&1; docker logs --tail %d -f -- %s", tail, shellQuote(name))
	h.streamContainerLogs(w, r, serverID, "container logs: "+name, func(ctx context.Context) (io.ReadCloser, error) {
		return entry.StreamCommand(ctx, "/bin/sh -c "+shellQuote(script))
	})
}

func (h *Hub) handleLocalContainerLogs(w http.ResponseWriter, r *http.Request, name string) {
	if !sshclient.ValidContainerName(name) {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	tail := logTail(r)
	h.streamContainerLogs(w, r, "__local__", "container logs: "+name, func(ctx context.Context) (io.ReadCloser, error) {
		return local.StreamLogs(ctx, name, tail)
	})
}

// streamContainerLogs upgrades to a WebSocket and pumps a merged docker log
// stream. The stream is cancelled when the browser closes the socket or the
// request context ends; the shared SSH connection is never closed.
func (h *Hub) streamContainerLogs(w http.ResponseWriter, r *http.Request, serverID, action string, open func(context.Context) (io.ReadCloser, error)) {
	if !h.acquireLogStream(serverID) {
		http.Error(w, "too many container log streams", http.StatusTooManyRequests)
		return
	}
	defer h.releaseLogStream(serverID)

	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(sshclient.WSReadLimit)
	sshclient.WSKeepalive(ws)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	connClosed := make(chan struct{})
	go func() {
		defer close(connClosed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	rc, err := open(ctx)
	if err != nil {
		ws.WriteMessage(websocket.BinaryMessage, []byte("[logs] "+err.Error()+"\r\n"))
		return
	}
	defer rc.Close()
	LogAction(serverID, action)

	buf := make([]byte, 16*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
		select {
		case <-connClosed:
			return
		default:
		}
	}
}

func (h *Hub) acquireLogStream(serverID string) bool {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logStreams[serverID] >= maxContainerLogStreams {
		return false
	}
	h.logStreams[serverID]++
	return true
}

func (h *Hub) releaseLogStream(serverID string) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if n := h.logStreams[serverID] - 1; n <= 0 {
		delete(h.logStreams, serverID)
	} else {
		h.logStreams[serverID] = n
	}
}

func cleanQuickActions(in []config.QuickAction) []config.QuickAction {
	return config.NormalizeQuickActions(in, true)
}

func cleanLocalQuickActions(in []config.QuickAction) []config.QuickAction {
	return config.NormalizeQuickActions(in, false)
}

// mergeQuickActionState carries last-run metadata forward when an action is
// edited but keeps the same stable ID (or, for legacy entries, the same label
// and command).
func mergeQuickActionState(old, next []config.QuickAction) {
	for i := range next {
		for _, prev := range old {
			if next[i].ID != "" && prev.ID == next[i].ID {
				next[i].LastRan = prev.LastRan
				next[i].LastStatus = prev.LastStatus
				break
			}
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
	oldQA := h.cfg.Servers[idx].QuickActions
	mergeQuickActionState(oldQA, qa)
	h.cfg.Servers[idx].QuickActions = qa
	if err := config.SaveAll(h.cfgPath, h.cfg); err != nil {
		h.cfg.Servers[idx].QuickActions = oldQA
		h.mu.Unlock()
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}
	updated := h.cfg.Servers[idx]
	online := false
	if entry := h.entries[id]; entry != nil {
		online = entry.GetStats().Online
	}
	h.mu.Unlock()

	writeJSON(w, toInfo(updated, online))
}

func (h *Hub) handleFavoriteServer(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
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
	old := h.cfg.Servers[idx].Favorite
	fav := body.Favorite
	h.cfg.Servers[idx].Favorite = &fav
	if err := config.SaveAll(h.cfgPath, h.cfg); err != nil {
		h.cfg.Servers[idx].Favorite = old
		h.mu.Unlock()
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}
	updated := h.cfg.Servers[idx]
	online := false
	if entry := h.entries[id]; entry != nil {
		online = entry.GetStats().Online
	}
	h.mu.Unlock()

	action := "removed from favorites"
	if fav {
		action = "added to favorites"
	}
	LogAction(id, action)
	writeJSON(w, toInfo(updated, online))
}

func (h *Hub) handleReboot(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host) {
	ctx, cancel := context.WithTimeout(r.Context(), powerActionTimeout)
	defer cancel()
	out, err := entry.RunCommand(ctx, "sudo reboot")
	LogAction(s.ID, "reboot")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleShutdown(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host) {
	ctx, cancel := context.WithTimeout(r.Context(), powerActionTimeout)
	defer cancel()
	out, err := entry.RunCommand(ctx, "sudo shutdown now")
	LogAction(s.ID, "shutdown")
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error(), "output": out})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (h *Hub) handleAction(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host, idxStr string) {
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
	if qa.Mode == "detached" {
		h.handleDetachedAction(w, r, entry, s.ID, idx, qa)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out, err := entry.RunCommand(ctx, qa.Command)
	status := "success"
	if err != nil {
		status = "error"
		if r.Context().Err() != nil {
			status = "stopped"
		}
	} else {
		// A quick action may pull/build images or recreate containers.
		entry.RefreshContainers()
		entry.RefreshImagesAsync()
	}
	h.recordQuickActionRun(s.ID, idx, qa.ID, qa.Label, qa.Command, status)
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

// handleDetachedAction starts a quick action as a tmux job and returns
// immediately. Output is available through the job Observe stream.
func (h *Hub) handleDetachedAction(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, serverID string, idx int, qa config.QuickAction) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	defer cancel()

	var err error
	if h.jobs == nil {
		err = fmt.Errorf("jobs unavailable")
	} else {
		var job jobs.Job
		job, err = h.jobs.Start(ctx, entry, serverID, jobs.StartRequest{
			Command: qa.Command,
			Label:   qa.Label,
			Session: qa.Session,
			Source:  "quick_action",
		})
		if err == nil {
			h.recordQuickActionRun(serverID, idx, qa.ID, qa.Label, qa.Command, "started")
			w.Header().Set("X-Quick-Action-Status", "started")
			w.Header().Set("X-Job-Id", job.ID)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Started job %s in session %s\n", job.ID, job.Session)
			return
		}
	}

	h.recordQuickActionRun(serverID, idx, qa.ID, qa.Label, qa.Command, "error")
	w.Header().Set("X-Quick-Action-Status", "error")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Error: %s\n", err)
}

// ── Detached tmux jobs ───────────────────────────────────────────────────────

const jobActionTimeout = 30 * time.Second

const (
	imageRefreshTimeout     = 2 * time.Minute
	maxContainerLogStreams  = 4
	defaultContainerLogTail = 200
)

func (h *Hub) handleListJobs(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID string) {
	if h.jobs == nil {
		http.Error(w, "jobs unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	defer cancel()
	if err := h.jobs.Refresh(ctx, runner, serverID); err != nil {
		// The cache is still useful while the host is offline; report it as-is.
		fmt.Printf("warning: could not refresh jobs for %s: %v\n", serverID, err)
	}
	writeJSON(w, h.jobs.List(serverID))
}

func (h *Hub) handleStartJob(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID string) {
	if h.jobs == nil {
		http.Error(w, "jobs unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req jobs.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if serverID == "__local__" {
		if lr, ok := runner.(*local.JobRunner); ok {
			req.Dir = lr.Dir()
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	defer cancel()
	job, err := h.jobs.Start(ctx, runner, serverID, req)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrInvalidJob):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, jobs.ErrSessionExists):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}
	LogAction(serverID, "job started: "+job.ID)
	writeJSON(w, job)
	h.triggerBroadcast()
}

func (h *Hub) handleJobRoute(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID, path string) {
	if h.jobs == nil {
		http.Error(w, "jobs unavailable", http.StatusServiceUnavailable)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		job, ok := h.jobs.Get(serverID, id)
		if !ok {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		writeJSON(w, job)

	case action == "stop" && r.Method == http.MethodPost:
		h.handleStopJob(w, r, runner, serverID, id)

	case action == "observe" && r.Method == http.MethodGet:
		h.handleObserveJob(w, r, runner, serverID, id)

	case action == "" && r.Method == http.MethodDelete:
		h.handleDismissJob(w, r, runner, serverID, id)

	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) handleStopJob(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	defer cancel()
	job, err := h.jobs.Stop(ctx, runner, serverID, id)
	if err != nil {
		if errors.Is(err, jobs.ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	LogAction(serverID, "job stopped: "+id)
	writeJSON(w, job)
}

func (h *Hub) handleDismissJob(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	defer cancel()
	if err := h.jobs.Dismiss(ctx, runner, serverID, id); err != nil {
		if errors.Is(err, jobs.ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	LogAction(serverID, "job dismissed: "+id)
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *Hub) handleObserveJob(w http.ResponseWriter, r *http.Request, runner jobs.Runner, serverID, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), jobActionTimeout)
	err := h.jobs.Refresh(ctx, runner, serverID)
	cancel()
	if err != nil {
		fmt.Printf("warning: could not refresh job %s/%s before observe: %v\n", serverID, id, err)
	}

	ch, unsubscribe, err := h.jobs.Subscribe(runner, serverID, id)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrJobNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, jobs.ErrTooManyObservers):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		default:
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}
	defer unsubscribe()

	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(sshclient.WSReadLimit)
	sshclient.WSKeepalive(ws)
	LogAction(serverID, "job observed: "+id)

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
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				return
			}
		}
	}
}

// recordQuickActionRun stores the last-run time and outcome for a quick
// action. The config may have been edited while the command was running, so
// the action is matched by label+command rather than blindly by index.
func (h *Hub) recordQuickActionRun(target string, idx int, id, label, command, status string) {
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
	if id != "" {
		for i := range *list {
			if (*list)[i].ID == id {
				found = i
				break
			}
		}
	}
	if found == -1 && idx >= 0 && idx < len(*list) && (*list)[idx].Label == label && (*list)[idx].Command == command {
		found = idx
	}
	if found == -1 {
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

	prevRan, prevStatus := (*list)[found].LastRan, (*list)[found].LastStatus
	(*list)[found].LastRan = time.Now().Unix()
	(*list)[found].LastStatus = status
	var saveErr error
	if target == "__local__" {
		saveErr = config.Save(h.cfgPath, h.cfg)
	} else {
		saveErr = config.SaveAll(h.cfgPath, h.cfg)
	}
	if saveErr != nil {
		(*list)[found].LastRan, (*list)[found].LastStatus = prevRan, prevStatus
		fmt.Printf("warning: could not save config: %v\n", saveErr)
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

// Upper bounds for short-lived remote actions. Container stops can legitimately
// take a while; power commands should return almost immediately (even if the
// connection drops as the host goes down).
const (
	containerActionTimeout = 60 * time.Second
	powerActionTimeout     = 30 * time.Second
)

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

	// Optional attach to a detached job: /api/server/<id>/terminal?job=<job-id>
	command := ""
	if jobID := strings.TrimSpace(r.URL.Query().Get("job")); jobID != "" {
		if h.jobs == nil {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		job, ok := h.jobs.Get(s.ID, jobID)
		if !ok || config.ValidateID(job.ID) != nil {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		command = "tmux attach -t " + shellQuote("serverhub-"+job.ID)
	}

	ws, err := h.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	sshclient.ProxyTerminal(ws, s.Host, s.Port, s.User, s.Key, command)
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
	Favorite     bool                 `json:"favorite"`
	QuickActions []config.QuickAction `json:"quick_actions"`
	Tmux         bool                 `json:"tmux"`
	Clock        dashClock            `json:"clock"`
}

func localInfo(hostname string, lc config.LocalConfig) localInfoResponse {
	name := lc.Name
	if name == "" {
		name = hostname
	}
	tags := lc.Tags
	if tags == nil {
		tags = []string{}
	}
	qa := lc.QuickActions
	if qa == nil {
		qa = []config.QuickAction{}
	}
	return localInfoResponse{
		ID:           "__local__",
		Name:         name,
		Hostname:     hostname,
		Online:       true,
		Tags:         tags,
		Notes:        lc.Notes,
		Favorite:     lc.Favorite == nil || *lc.Favorite,
		QuickActions: qa,
		Tmux:         local.TmuxAvailable(),
		Clock:        newDashClock(),
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
			Favorite     *bool                 `json:"favorite"`
			QuickActions *[]config.QuickAction `json:"quick_actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		previous := h.cfg.Local
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
		if body.Favorite != nil {
			h.cfg.Local.Favorite = body.Favorite
		}
		if body.QuickActions != nil {
			qa := cleanLocalQuickActions(*body.QuickActions)
			mergeQuickActionState(h.cfg.Local.QuickActions, qa)
			h.cfg.Local.QuickActions = qa
		}
		if err := config.Save(h.cfgPath, h.cfg); err != nil {
			h.cfg.Local = previous
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
	h.recordQuickActionRun("__local__", idx, action.ID, action.Label, action.Command, status)
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

func (h *Hub) handleLocalImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	images, err := local.CollectImages()
	if err != nil {
		http.Error(w, "docker unavailable", http.StatusInternalServerError)
		return
	}
	if images == nil {
		images = []sshclient.Image{}
	}
	writeJSON(w, sshclient.ImageList{
		UpdatedAt: time.Now().Unix(),
		Images:    images,
	})
}

func (h *Hub) handleLocalContainer(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/local/container/"), "/", 2)
	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		h.handleLocalContainerLogs(w, r, parts[0])
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
		Hue                    int                   `json:"hue"`
		Shade                  int                   `json:"shade"`
		BackgroundColor        string                `json:"background_color"`
		BackgroundColorEnabled bool                  `json:"background_color_enabled"`
		SurfaceColor           string                `json:"surface_color"`
		SurfaceColorEnabled    bool                  `json:"surface_color_enabled"`
		SurfaceShade           int                   `json:"surface_shade"`
		Animations             bool                  `json:"animations"`
		Terminal               config.TerminalConfig `json:"terminal"`
		Owner                  string                `json:"owner"`
		SizeUnits              string                `json:"size_units"`
		BackgroundImage        string                `json:"background_image,omitempty"`
		BackgroundBlur         int                   `json:"background_blur"`
		DashboardOrder         []string              `json:"dashboard_order"`
		DashboardHidden        []string              `json:"dashboard_hidden"`
		DashboardDays          bool                  `json:"dashboard_days"`
	}

	if r.Method == http.MethodGet {
		h.mu.RLock()
		resp := appConfigResponse{
			Hue:                    h.hue,
			Shade:                  h.shade,
			BackgroundColor:        h.cfg.BackgroundColor,
			BackgroundColorEnabled: colorEnabled(h.cfg.BackgroundColorEnabled),
			SurfaceColor:           h.cfg.SurfaceColor,
			SurfaceColorEnabled:    colorEnabled(h.cfg.SurfaceColorEnabled),
			SurfaceShade:           effectiveSurfaceShade(h.cfg),
			Animations:             h.animations,
			Terminal:               h.cfg.Terminal,
			Owner:                  h.cfg.Owner,
			SizeUnits:              effectiveSizeUnits(h.cfg),
			BackgroundImage:        h.cfg.BackgroundImage,
			BackgroundBlur:         h.cfg.BackgroundBlur,
			DashboardOrder:         normalizeDashboardOrder(h.cfg.DashboardOrder),
			DashboardHidden:        normalizeDashboardHidden(h.cfg.DashboardHidden),
			DashboardDays:          dashboardDays(h.cfg),
		}
		h.mu.RUnlock()
		writeJSON(w, resp)
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			Hue                    *int      `json:"hue"`
			Shade                  *int      `json:"shade"`
			BackgroundColor        *string   `json:"background_color"`
			BackgroundColorEnabled *bool     `json:"background_color_enabled"`
			SurfaceColor           *string   `json:"surface_color"`
			SurfaceColorEnabled    *bool     `json:"surface_color_enabled"`
			SurfaceShade           *int      `json:"surface_shade"`
			Animations             *bool     `json:"animations"`
			SizeUnits              *string   `json:"size_units"`
			BackgroundBlur         *int      `json:"background_blur"`
			BackgroundImage        *string   `json:"background_image"`
			DashboardOrder         *[]string `json:"dashboard_order"`
			DashboardHidden        *[]string `json:"dashboard_hidden"`
			DashboardDays          *bool     `json:"dashboard_days"`
			Terminal               *struct {
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
		prevHue, prevShade, prevAnimations := h.hue, h.shade, h.animations
		prevCfg := h.cfg
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
		if body.SizeUnits != nil {
			if *body.SizeUnits != "GB" && *body.SizeUnits != "GiB" {
				h.mu.Unlock()
				http.Error(w, "size_units must be GB or GiB", http.StatusBadRequest)
				return
			}
			h.cfg.SizeUnits = *body.SizeUnits
		}
		if body.BackgroundBlur != nil && *body.BackgroundBlur >= 0 && *body.BackgroundBlur <= 60 {
			h.cfg.BackgroundBlur = *body.BackgroundBlur
		}
		if body.BackgroundColor != nil && config.ValidHexColor(*body.BackgroundColor) {
			h.cfg.BackgroundColor = *body.BackgroundColor
		}
		if body.SurfaceColor != nil && config.ValidHexColor(*body.SurfaceColor) {
			h.cfg.SurfaceColor = *body.SurfaceColor
		}
		if body.BackgroundColorEnabled != nil {
			v := *body.BackgroundColorEnabled
			h.cfg.BackgroundColorEnabled = &v
		}
		if body.SurfaceColorEnabled != nil {
			v := *body.SurfaceColorEnabled
			h.cfg.SurfaceColorEnabled = &v
		}
		if body.SurfaceShade != nil && *body.SurfaceShade >= 0 && *body.SurfaceShade <= 100 {
			v := *body.SurfaceShade
			h.cfg.SurfaceShade = &v
		}
		if body.DashboardOrder != nil {
			h.cfg.DashboardOrder = normalizeDashboardOrder(*body.DashboardOrder)
		}
		if body.DashboardHidden != nil {
			h.cfg.DashboardHidden = normalizeDashboardHidden(*body.DashboardHidden)
		}
		if body.DashboardDays != nil {
			v := *body.DashboardDays
			h.cfg.DashboardDays = &v
		}
		if body.BackgroundImage != nil {
			name := strings.TrimSpace(*body.BackgroundImage)
			if name != "" && !media.Exists(name) {
				h.mu.Unlock()
				http.Error(w, "background image not found", http.StatusBadRequest)
				return
			}
			h.cfg.BackgroundImage = name
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
			h.hue, h.shade, h.animations = prevHue, prevShade, prevAnimations
			h.cfg = prevCfg
			h.mu.Unlock()
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		resp := appConfigResponse{
			Hue:                    h.hue,
			Shade:                  h.shade,
			BackgroundColor:        h.cfg.BackgroundColor,
			BackgroundColorEnabled: colorEnabled(h.cfg.BackgroundColorEnabled),
			SurfaceColor:           h.cfg.SurfaceColor,
			SurfaceColorEnabled:    colorEnabled(h.cfg.SurfaceColorEnabled),
			SurfaceShade:           effectiveSurfaceShade(h.cfg),
			Animations:             h.animations,
			Terminal:               h.cfg.Terminal,
			Owner:                  h.cfg.Owner,
			SizeUnits:              effectiveSizeUnits(h.cfg),
			BackgroundImage:        h.cfg.BackgroundImage,
			BackgroundBlur:         h.cfg.BackgroundBlur,
			DashboardOrder:         normalizeDashboardOrder(h.cfg.DashboardOrder),
			DashboardHidden:        normalizeDashboardHidden(h.cfg.DashboardHidden),
			DashboardDays:          dashboardDays(h.cfg),
		}
		h.mu.Unlock()
		writeJSON(w, resp)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func colorEnabled(v *bool) bool { return v == nil || *v }

func effectiveSizeUnits(cfg *config.Config) string {
	if cfg.SizeUnits == "GB" || cfg.SizeUnits == "GiB" {
		return cfg.SizeUnits
	}
	return "GiB"
}

func effectiveSurfaceShade(cfg *config.Config) int {
	if cfg.SurfaceShade != nil {
		v := *cfg.SurfaceShade
		if v < 0 {
			return 0
		}
		if v > 100 {
			return 100
		}
		return v
	}
	bg := cfg.Shade
	if bg <= 46 {
		if bg+3 > 100 {
			return 100
		}
		return bg + 3
	}
	if bg-3 < 2 {
		return 2
	}
	return bg - 3
}

func dashboardDays(cfg *config.Config) bool {
	return cfg.DashboardDays == nil || *cfg.DashboardDays
}

func normalizeDashboardHidden(in []string) []string {
	allowed := map[string]bool{"apps": true, "favorites": true, "quick_actions": true}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if !allowed[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func normalizeDashboardOrder(in []string) []string {
	allowed := map[string]bool{"apps": true, "favorites": true, "quick_actions": true}
	out := make([]string, 0, 3)
	seen := make(map[string]bool, 3)
	for _, id := range in {
		id = strings.TrimSpace(id)
		if !allowed[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range []string{"apps", "favorites", "quick_actions"} {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
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
		var snapshot *config.TerminalConfig
		if *override != nil {
			c := **override
			snapshot = &c
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
		var saveErr error
		if target == "__local__" {
			saveErr = config.Save(h.cfgPath, h.cfg)
		} else {
			saveErr = config.SaveAll(h.cfgPath, h.cfg)
		}
		if saveErr != nil {
			*override = snapshot
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
		}
		writeJSON(w, terminalConfigResponse{
			Effective: mergeTerminalConfig(h.cfg.Terminal, *override),
			Override:  *override,
		})

	case http.MethodDelete:
		snapshot := *override
		*override = nil
		var saveErr error
		if target == "__local__" {
			saveErr = config.Save(h.cfgPath, h.cfg)
		} else {
			saveErr = config.SaveAll(h.cfgPath, h.cfg)
		}
		if saveErr != nil {
			*override = snapshot
			http.Error(w, "could not save config", http.StatusInternalServerError)
			return
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

// ── Network probes ───────────────────────────────────────────────────────────

const (
	probePingTimeout   = 30 * time.Second
	probeDNSTimeout    = 15 * time.Second
	probeHTTPTimeout   = 25 * time.Second
	probeTraceTimeout  = 35 * time.Second
	probeIperfTimeout  = 60 * time.Second
	probeIperfSeconds  = 5
	probeIperfMinPort  = 20000
	probeIperfPortSpan = 40000
)

type probeRequest struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

func writeErrorJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *Hub) handleProbe(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	typ, err := probe.NormalizeType(req.Type)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		writeErrorJSON(w, http.StatusBadRequest, "target is required")
		return
	}
	if typ == probe.TypeIperf3 {
		h.handleIperfProbe(w, r, entry, s, target)
		return
	}

	// A configured server ID is resolved to its host for host-based probes.
	targetEntry, targetCfg, isServer := h.serverByID(target)
	hostTarget := target
	if isServer && typ != probe.TypeHTTP {
		hostTarget = targetCfg.Host
	}
	if typ == probe.TypeHTTP {
		if err := probe.ValidateURL(target); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if err := probe.ValidateHost(hostTarget); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// Ping a configured server from both ends: A→B and B→A.
	if typ == probe.TypePing && isServer && targetEntry != nil {
		ctx, cancel := context.WithTimeout(r.Context(), probePingTimeout)
		defer cancel()
		start := time.Now()
		forward := probe.Run(ctx, entry, probe.Request{Type: typ, Target: targetCfg.Host})
		reverse := probe.Run(ctx, targetEntry, probe.Request{Type: typ, Target: s.Host})
		result := combinePing(target, targetCfg.Host, s.Host, forward, reverse)
		result.DurationMS = time.Since(start).Milliseconds()
		LogAction(s.ID, "probe ping: "+target+" (both directions)")
		writeJSON(w, result)
		return
	}

	timeout := probePingTimeout
	switch typ {
	case probe.TypeDNS:
		timeout = probeDNSTimeout
	case probe.TypeHTTP:
		timeout = probeHTTPTimeout
	case probe.TypeTraceroute:
		timeout = probeTraceTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	result := probe.Run(ctx, entry, probe.Request{Type: typ, Target: hostTarget})
	result.Target = target
	LogAction(s.ID, "probe "+typ+": "+target)
	writeJSON(w, result)
}

func combinePing(target, targetHost, sourceHost string, forward, reverse probe.Result) probe.Result {
	result := forward
	result.Type = probe.TypePing
	result.Target = target
	data := make(map[string]interface{})
	if p, ok := forward.Data["ping"]; ok {
		data["forward"] = p
	}
	if p, ok := reverse.Data["ping"]; ok {
		data["reverse"] = p
	}
	if len(data) > 0 {
		result.Data = data
	} else {
		result.Data = nil
	}

	switch {
	case forward.Status == "success" && reverse.Status == "success":
		result.Status = "success"
	case forward.Status == "success" || reverse.Status == "success":
		result.Status = "partial"
	default:
		result.Status = "error"
	}
	result.Summary = "→ " + forward.Summary + " | ← " + reverse.Summary
	result.Output = "Forward (" + targetHost + "):\n" + forward.Output +
		"\n\nReverse (" + sourceHost + "):\n" + reverse.Output
	var errs []string
	if forward.Error != "" {
		errs = append(errs, "forward: "+forward.Error)
	}
	if reverse.Error != "" {
		errs = append(errs, "reverse: "+reverse.Error)
	}
	result.Error = strings.Join(errs, "; ")
	result.StartedAt = forward.StartedAt
	return result
}

func (h *Hub) handleIperfProbe(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry, s *config.Host, targetID string) {
	targetEntry, targetCfg, ok := h.serverByID(targetID)
	if !ok || targetEntry == nil {
		writeErrorJSON(w, http.StatusBadRequest, "iperf3 target must be a configured server ID")
		return
	}
	if targetID == s.ID {
		writeErrorJSON(w, http.StatusBadRequest, "choose a different server for the iperf3 target")
		return
	}
	if h.jobs == nil {
		http.Error(w, "jobs unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeIperfTimeout)
	defer cancel()
	if !entry.GetInfo().Capabilities.Iperf3 && !hasIperf3(ctx, entry) {
		writeErrorJSON(w, http.StatusBadRequest, "iperf3 is not installed on "+s.ID)
		return
	}
	if !targetEntry.GetInfo().Capabilities.Iperf3 && !hasIperf3(ctx, targetEntry) {
		writeErrorJSON(w, http.StatusBadRequest, "iperf3 is not installed on "+targetID)
		return
	}

	port := probeIperfMinPort + rand.Intn(probeIperfPortSpan)
	serverJob, err := h.jobs.Start(ctx, targetEntry, targetID, jobs.StartRequest{
		Command: probe.BuildIperfServer(port),
		Label:   "iperf3 server · " + s.ID + " → " + targetID,
		Source:  "probe",
	})
	if err != nil {
		writeErrorJSON(w, http.StatusBadGateway, "could not start iperf3 server: "+err.Error())
		return
	}
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := h.jobs.Dismiss(cleanup, targetEntry, targetID, serverJob.ID); err != nil {
			fmt.Printf("warning: could not clean up iperf3 job %s: %v\n", serverJob.ID, err)
		}
	}()

	// Give the one-shot server a moment to bind, then run the client. One retry
	// covers startup races without making the test long.
	if !sleepCtx(ctx, 800*time.Millisecond) {
		writeErrorJSON(w, http.StatusGatewayTimeout, "probe timed out")
		return
	}
	start := time.Now()
	out, runErr := entry.RunCommand(ctx, probe.BuildIperfClient(targetCfg.Host, port, probeIperfSeconds))
	if runErr != nil && sleepCtx(ctx, 700*time.Millisecond) {
		out, runErr = entry.RunCommand(ctx, probe.BuildIperfClient(targetCfg.Host, port, probeIperfSeconds))
	}
	result := probe.Result{
		Type:       probe.TypeIperf3,
		Target:     targetID,
		StartedAt:  start.Unix(),
		DurationMS: time.Since(start).Milliseconds(),
		Output:     out,
	}
	if runErr != nil {
		result.Status = "error"
		result.Error = runErr.Error()
		result.Summary = "iperf3 client failed"
		LogAction(s.ID, "probe iperf3 failed: "+s.ID+" → "+targetID)
		writeJSON(w, result)
		return
	}
	data, err := probe.ParseIperf3(out)
	if err != nil {
		result.Status = "error"
		result.Error = err.Error()
		result.Summary = "could not parse iperf3 output"
		writeJSON(w, result)
		return
	}
	data.Port = port
	result.Status = "success"
	result.Data = map[string]interface{}{"iperf": data, "server_job": serverJob.ID}
	result.Summary = fmt.Sprintf("sent %.1f Mbps, received %.1f Mbps", data.SentMbps, data.RecvMbps)
	LogAction(s.ID, "probe iperf3: "+s.ID+" → "+targetID)
	writeJSON(w, result)
}

func hasIperf3(ctx context.Context, runner probe.Runner) bool {
	out, err := runner.RunCommand(ctx, "/bin/sh -c 'command -v iperf3'")
	return err == nil && strings.TrimSpace(out) != ""
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (h *Hub) serverByID(id string) (*sshclient.ServerEntry, config.Host, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	entry := h.entries[id]
	for i := range h.cfg.Servers {
		if h.cfg.Servers[i].ID == id {
			return entry, h.cfg.Servers[i], true
		}
	}
	return nil, config.Host{}, false
}

// ── Schedules ────────────────────────────────────────────────────────────────

func (h *Hub) handleSchedules(w http.ResponseWriter, r *http.Request) {
	if h.sched == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := h.sched.List()
		if h.jobs != nil {
			for i := range list {
				for serverID, jobID := range list[i].LastJobs {
					if job, ok := h.jobs.Get(serverID, jobID); ok && job.Status == jobs.StatusRunning {
						list[i].Running = true
					}
				}
			}
		}
		writeJSON(w, list)

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var def scheduler.Definition
		if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		view, err := h.sched.Create(def)
		if err != nil {
			scheduleError(w, err)
			return
		}
		LogAction("scheduler", "schedule created: "+view.ID)
		writeJSON(w, view)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if h.sched == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/schedules/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		view, ok := h.sched.Get(id)
		if !ok {
			http.Error(w, scheduler.ErrNotFound.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, view)

	case action == "" && r.Method == http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var def scheduler.Definition
		if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		view, err := h.sched.Update(id, def)
		if err != nil {
			scheduleError(w, err)
			return
		}
		LogAction("scheduler", "schedule updated: "+id)
		writeJSON(w, view)

	case action == "" && r.Method == http.MethodPatch:
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Enabled == nil {
			http.Error(w, "enabled is required", http.StatusBadRequest)
			return
		}
		view, err := h.sched.SetEnabled(id, *body.Enabled)
		if err != nil {
			scheduleError(w, err)
			return
		}
		LogAction("scheduler", "schedule enabled changed: "+id)
		writeJSON(w, view)

	case action == "" && r.Method == http.MethodDelete:
		if err := h.sched.Delete(id); err != nil {
			scheduleError(w, err)
			return
		}
		LogAction("scheduler", "schedule deleted: "+id)
		writeJSON(w, map[string]bool{"ok": true})

	case action == "delete-occurrence" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		var body struct {
			Time   int64 `json:"time"`
			Future bool  `json:"future"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		view, err := h.sched.DeleteOccurrence(id, body.Time, body.Future)
		if err != nil {
			scheduleError(w, err)
			return
		}
		scope := "one occurrence"
		if body.Future {
			scope = "this and future occurrences"
		}
		LogAction("scheduler", "schedule "+id+": deleted "+scope)
		writeJSON(w, view)

	case action == "run" && r.Method == http.MethodPost:
		// Validate up front so unknown ids still 404, then dispatch. The run
		// proceeds in the background (jobs are detached anyway) so the
		// browser is not held up to two minutes: startTarget does serial
		// blocking tmux refreshes per target.
		if _, ok := h.sched.Get(id); !ok {
			http.Error(w, scheduler.ErrNotFound.Error(), http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		go func() {
			defer cancel()
			if _, err := h.sched.RunNow(ctx, id); err != nil {
				LogAction("scheduler", "schedule run now error: "+id+": "+err.Error())
			}
		}()
		LogAction("scheduler", "schedule run now: "+id)
		writeJSON(w, map[string]string{"status": "dispatched"})

	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) handleScheduleOccurrences(w http.ResponseWriter, r *http.Request) {
	if h.sched == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	from, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), time.Local)
	if err != nil {
		http.Error(w, "from must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	to, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), time.Local)
	if err != nil {
		http.Error(w, "to must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if !to.After(from) {
		http.Error(w, "to must be after from", http.StatusBadRequest)
		return
	}
	if to.Sub(from) > 366*24*time.Hour {
		http.Error(w, "range is limited to 366 days", http.StatusBadRequest)
		return
	}
	writeJSON(w, h.sched.Occurrences(from, to))
}

func (h *Hub) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if h.sched == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var def scheduler.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	preview, err := h.sched.Preview(def)
	if err != nil {
		scheduleError(w, err)
		return
	}
	writeJSON(w, preview)
}

func scheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, scheduler.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, scheduler.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ScheduleTargets snapshots the current inventory for the scheduler.
func (h *Hub) ScheduleTargets() []scheduler.Target {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]scheduler.Target, 0, len(h.cfg.Servers)+1)
	out = append(out, scheduler.Target{
		ID:           "__local__",
		Tags:         append([]string(nil), h.cfg.Local.Tags...),
		QuickActions: append([]config.QuickAction(nil), h.cfg.Local.QuickActions...),
	})
	for _, s := range h.cfg.Servers {
		out = append(out, scheduler.Target{
			ID:           s.ID,
			Tags:         append([]string(nil), s.Tags...),
			QuickActions: append([]config.QuickAction(nil), s.QuickActions...),
		})
	}
	return out
}

// RunScheduleJob starts a job through the normal jobs registry.
func (h *Hub) RunScheduleJob(ctx context.Context, serverID string, req jobs.StartRequest) (jobs.Job, error) {
	h.mu.RLock()
	entry := h.entries[serverID]
	jobReg := h.jobs
	localRunner := h.localJobs
	h.mu.RUnlock()
	if jobReg == nil {
		return jobs.Job{}, fmt.Errorf("jobs unavailable")
	}
	if serverID == "__local__" {
		if localRunner == nil {
			return jobs.Job{}, fmt.Errorf("local detached jobs require tmux")
		}
		req.Dir = localRunner.Dir()
		return jobReg.Start(ctx, localRunner, serverID, req)
	}
	if entry == nil {
		return jobs.Job{}, fmt.Errorf("server %q not found", serverID)
	}
	return jobReg.Start(ctx, entry, serverID, req)
}

// ReconcileScheduleJobs refreshes job status for servers that still have
// running schedule-sourced jobs, so finished commands update schedule history
// (via the registry's change hook) without waiting for someone to view the
// jobs list. Intended for a slow background ticker; errors are ignored
// (offline targets simply retry next pass).
func (h *Hub) ReconcileScheduleJobs() {
	h.mu.RLock()
	jobReg := h.jobs
	localRunner := h.localJobs
	entries := h.entries
	h.mu.RUnlock()
	if jobReg == nil {
		return
	}
	for _, serverID := range jobReg.ServersWithRunningScheduleJobs() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if serverID == "__local__" {
			if localRunner != nil {
				_ = jobReg.Refresh(ctx, localRunner, serverID)
			}
		} else if entry := entries[serverID]; entry != nil {
			_ = jobReg.Refresh(ctx, entry, serverID)
		}
		cancel()
	}
}

// ScheduleJobStatus refreshes and returns a job's status (overlap checks).
func (h *Hub) ScheduleJobStatus(serverID, jobID string) (string, bool) {
	h.mu.RLock()
	entry := h.entries[serverID]
	jobReg := h.jobs
	localRunner := h.localJobs
	h.mu.RUnlock()
	if jobReg == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serverID == "__local__" {
		if localRunner == nil {
			return "", false
		}
		_ = jobReg.Refresh(ctx, localRunner, serverID)
	} else {
		if entry == nil {
			return "", false
		}
		_ = jobReg.Refresh(ctx, entry, serverID)
	}
	job, ok := jobReg.Get(serverID, jobID)
	if !ok {
		return "", false
	}
	return job.Status, true
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
		ID:       s.ID,
		Name:     s.Name,
		Host:     s.Host,
		Port:     s.Port,
		User:     s.User,
		Key:      s.Key,
		Tags:     tags,
		Notes:    s.Notes,
		Online:   online,
		Favorite: s.Favorite != nil && *s.Favorite,
		QA:       qa,
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

func validPort(port int) bool { return port >= 1 && port <= 65535 }

// isLoopbackRequest reports whether the request originated from this machine.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
