package ssh

import (
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"serverhub/internal/debug"
)

const (
	ringSize     = 360 // 30 min at 5s
	pollInterval = 5 * time.Second
)

// loadPattern allows only numeric load values (digits, dot, comma). The load
// strings originate from the remote host's /proc/loadavg and are rendered in
// the dashboard — anything outside this charset is dropped (XSS containment).
var loadPattern = regexp.MustCompile(`^[0-9.,]+$`)

func sanitizeLoad(s string) string {
	if s == "" || len(s) > 32 || !loadPattern.MatchString(s) {
		return ""
	}
	return s
}

type StatPoint struct {
	Time    int64   `json:"t"`
	CPU     float64 `json:"cpu"`
	MemPct  float64 `json:"mem_pct"`
	DiskPct float64 `json:"disk_pct"`
}

type ServerStats struct {
	Online    bool        `json:"online"`
	UpdatedAt int64       `json:"updated_at"`
	Uptime    int64       `json:"uptime"`
	CPU       float64     `json:"cpu"`
	MemTotal  int64       `json:"mem_total"`
	MemUsed   int64       `json:"mem_used"`
	MemPct    float64     `json:"mem_pct"`
	DiskUsed  int64       `json:"disk_used"`
	DiskTotal int64       `json:"disk_total"`
	DiskPct   float64     `json:"disk_pct"`
	Load1     string      `json:"load1"`
	Load5     string      `json:"load5"`
	Load15    string      `json:"load15"`
	History   []StatPoint `json:"history"`
}

type ContainerList struct {
	UpdatedAt  int64       `json:"updated_at"`
	Containers []Container `json:"containers"`
}

type ServerEntry struct {
	ID      string
	Host    string
	Port    int
	User    string
	KeyPath string

	// OnUpdate is called (non-blocking) after every stats or container poll.
	// Set this before calling StartOne; safe to leave nil.
	OnUpdate func()

	mu         sync.RWMutex
	stats      ServerStats
	ring       [ringSize]StatPoint
	ringHead   int
	ringCount  int
	containers ContainerList
	done       chan struct{}
}

func NewServerEntry(id, host string, port int, user, keyPath string) *ServerEntry {
	return &ServerEntry{
		ID:      id,
		Host:    host,
		Port:    port,
		User:    user,
		KeyPath: keyPath,
		done:    make(chan struct{}),
	}
}

// Stop signals the polling goroutine to exit.
func (e *ServerEntry) Stop() {
	close(e.done)
}

func (e *ServerEntry) GetStats() ServerStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.stats
	s.History = e.getHistory()
	return s
}

func (e *ServerEntry) GetContainers() ContainerList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.containers
}

func (e *ServerEntry) getHistory() []StatPoint {
	if e.ringCount == 0 {
		return []StatPoint{}
	}
	out := make([]StatPoint, e.ringCount)
	start := (e.ringHead - e.ringCount + ringSize) % ringSize
	for i := 0; i < e.ringCount; i++ {
		out[i] = e.ring[(start+i)%ringSize]
	}
	return out
}

func (e *ServerEntry) poll() {
	start := time.Now()
	raw, err := CollectStats(e.Host, e.Port, e.User, e.KeyPath)
	debug.Global.RecordTiming("ssh.stats."+e.ID, time.Since(start))

	e.mu.Lock()

	if err != nil {
		log.Printf("[%s] stats poll failed: %v", e.ID, err)
		e.stats.Online = false
		e.stats.UpdatedAt = time.Now().Unix()
		onUpdate := e.OnUpdate
		e.mu.Unlock()
		if onUpdate != nil {
			onUpdate()
		}
		return
	}

	memUsed := raw.MemTotal - raw.MemAvail
	var memPct float64
	if raw.MemTotal > 0 {
		memPct = math.Round(float64(memUsed)/float64(raw.MemTotal)*1000) / 10
	}

	parts := strings.Fields(raw.Disk)
	var diskUsed, diskTotal int64
	if len(parts) >= 2 {
		diskUsed, _ = strconv.ParseInt(parts[0], 10, 64)
		diskTotal, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	var diskPct float64
	if diskTotal > 0 {
		diskPct = math.Round(float64(diskUsed)/float64(diskTotal)*1000) / 10
	}

	loadParts := strings.Fields(raw.Load)
	load1, load5, load15 := "", "", ""
	if len(loadParts) >= 3 {
		load1 = sanitizeLoad(loadParts[0])
		load5 = sanitizeLoad(loadParts[1])
		load15 = sanitizeLoad(loadParts[2])
	}

	cpuPct := float64(raw.CPU)

	e.stats = ServerStats{
		Online:    true,
		UpdatedAt: time.Now().Unix(),
		Uptime:    raw.Uptime,
		CPU:       cpuPct,
		MemTotal:  raw.MemTotal,
		MemUsed:   memUsed,
		MemPct:    memPct,
		DiskUsed:  diskUsed,
		DiskTotal: diskTotal,
		DiskPct:   diskPct,
		Load1:     load1,
		Load5:     load5,
		Load15:    load15,
	}

	pt := StatPoint{
		Time:    time.Now().Unix(),
		CPU:     cpuPct,
		MemPct:  memPct,
		DiskPct: diskPct,
	}
	e.ring[e.ringHead] = pt
	e.ringHead = (e.ringHead + 1) % ringSize
	if e.ringCount < ringSize {
		e.ringCount++
	}
	onUpdate := e.OnUpdate
	e.mu.Unlock()
	if onUpdate != nil {
		onUpdate()
	}
}

func (e *ServerEntry) pollContainers() {
	start := time.Now()
	containers, err := CollectContainers(e.Host, e.Port, e.User, e.KeyPath)
	debug.Global.RecordTiming("ssh.docker."+e.ID, time.Since(start))

	e.mu.Lock()
	if err != nil {
		log.Printf("[%s] docker poll failed: %v", e.ID, err)
		e.mu.Unlock()
		return
	}
	if containers == nil {
		containers = []Container{}
	}
	e.containers = ContainerList{
		UpdatedAt:  time.Now().Unix(),
		Containers: containers,
	}
	onUpdate := e.OnUpdate
	e.mu.Unlock()
	if onUpdate != nil {
		onUpdate()
	}
}

// StartOne begins polling a single server entry in the background.
func StartOne(e *ServerEntry) {
	go func() {
		e.poll()
		e.pollContainers()
		statTick := time.NewTicker(pollInterval)
		dockerTick := time.NewTicker(pollInterval)
		defer statTick.Stop()
		defer dockerTick.Stop()
		for {
			select {
			case <-e.done:
				return
			case <-statTick.C:
				e.poll()
			case <-dockerTick.C:
				e.pollContainers()
			}
		}
	}()
}

func StartPolling(entries []*ServerEntry) {
	for _, entry := range entries {
		StartOne(entry)
	}
}
