package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	sshclient "serverhub/internal/ssh"
)

// dashCard is the compact per-server payload sent over the SSE stream.
// It carries only what the dashboard cards need — no history, no raw bytes.
type dashCard struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Online  bool    `json:"online"`
	CPU     float64 `json:"cpu"`
	MemPct  float64 `json:"mem_pct"`
	DiskPct float64 `json:"disk_pct"`
	Uptime  int64   `json:"uptime"`
	Load1   string  `json:"load1"`
}

// dashClock reports the hub process's wall clock and UTC offset so the UI can
// relate schedule times to the viewer's own clock when they differ.
type dashClock struct {
	Time             int64  `json:"time"`
	UtcOffsetMinutes int    `json:"utc_offset_minutes"`
	TzName           string `json:"tz_name"`
}

func newDashClock() dashClock {
	now := time.Now()
	name, offset := now.Zone()
	return dashClock{
		Time:             now.Unix(),
		UtcOffsetMinutes: offset / 60,
		TzName:           name,
	}
}

type dashSnapshot struct {
	Local   *dashCard  `json:"local,omitempty"`
	Servers []dashCard `json:"servers"`
	Clock   dashClock  `json:"clock"`
}

// sseHub manages SSE client subscriptions.
type sseHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[chan []byte]struct{})}
}

func (s *sseHub) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *sseHub) unsubscribe(ch chan []byte) {
	s.mu.Lock()
	delete(s.clients, ch)
	s.mu.Unlock()
}

func (s *sseHub) broadcast(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- data:
		default: // slow client — skip this tick
		}
	}
}

// buildDashSnapshot assembles a compact snapshot of all dashboard cards.
func (h *Hub) buildDashSnapshot() dashSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	snap := dashSnapshot{
		Servers: make([]dashCard, 0, len(h.cfg.Servers)),
		Clock:   newDashClock(),
	}

	// Local machine card
	localStats := h.local.GetStats()
	localName := h.cfg.Local.Name
	if localName == "" {
		localName = h.local.Hostname()
	}
	snap.Local = &dashCard{
		ID:      "__local__",
		Name:    localName,
		Online:  true,
		CPU:     localStats.CPU,
		MemPct:  localStats.MemPct,
		DiskPct: localStats.DiskPct,
		Uptime:  localStats.Uptime,
		Load1:   localStats.Load1,
	}

	// Remote server cards
	for _, s := range h.cfg.Servers {
		card := dashCard{ID: s.ID, Name: s.Name}
		if entry := h.entries[s.ID]; entry != nil {
			st := entry.GetStats()
			card.Online = st.Online
			card.CPU = st.CPU
			card.MemPct = st.MemPct
			card.DiskPct = st.DiskPct
			card.Uptime = st.Uptime
			card.Load1 = st.Load1
		}
		snap.Servers = append(snap.Servers, card)
	}

	return snap
}

// triggerBroadcast signals the broadcaster goroutine to push a snapshot
// immediately (e.g. after a server is added or removed).
func (h *Hub) triggerBroadcast() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// runBroadcaster pushes a dashboard snapshot to all SSE clients every 5 s,
// and also whenever triggerBroadcast is called.
func (h *Hub) runBroadcaster() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.broadcastSnapshot()
		case <-h.notify:
			h.broadcastSnapshot()
		}
	}
}

func (h *Hub) broadcastSnapshot() {
	snap := h.buildDashSnapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	h.sse.broadcast(data)
}

// handleDashboardEvents serves the SSE stream at GET /api/dashboard/events.
func (h *Hub) handleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied

	ch := h.sse.subscribe()
	defer h.sse.unsubscribe(ch)

	// Send current snapshot immediately so the client doesn't wait up to 5 s.
	snap := h.buildDashSnapshot()
	if data, err := json.Marshal(snap); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// ── Per-server detail event stream ───────────────────────────────────────────

type serverDetailEvent struct {
	Stats      sshclient.ServerStats `json:"stats"`
	Containers []sshclient.Container `json:"containers"`
}

// handleServerEvents streams full stats + containers for a single server.
// Used by the detail view to replace the setInterval poll.
func (h *Hub) handleServerEvents(w http.ResponseWriter, r *http.Request, entry *sshclient.ServerEntry) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func() bool {
		evt := serverDetailEvent{
			Stats:      entry.GetStats(),
			Containers: entry.GetContainers().Containers,
		}
		if evt.Containers == nil {
			evt.Containers = []sshclient.Container{}
		}
		data, err := json.Marshal(evt)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return err == nil
	}

	if !send() {
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// handleLocalEvents streams local machine stats for the local detail view.
func (h *Hub) handleLocalEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	send := func() bool {
		data, err := json.Marshal(h.local.GetStats())
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return err == nil
	}

	if !send() {
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
