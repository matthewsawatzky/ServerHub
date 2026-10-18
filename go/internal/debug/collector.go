package debug

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxLogs    = 2000
	maxTimings = 500
	// maxStringLength bounds any single recorded string (log line, URL path)
	// so crafted huge values cannot grow entries unboundedly.
	maxStringLength = 512
)

// truncate bounds s to maxStringLength runes.
func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxStringLength {
		return s
	}
	return string(r[:maxStringLength]) + "…"
}

// ── Types ─────────────────────────────────────────────────────────────────────

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

type LogEntry struct {
	T      int64  `json:"t"`
	Level  Level  `json:"level"`
	Source string `json:"source"`
	Msg    string `json:"msg"`
}

type TimingEntry struct {
	T    int64   `json:"t"`
	Name string  `json:"name"`
	Ms   float64 `json:"ms"`
}

type Stats struct {
	// Runtime memory
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapSysMB   float64 `json:"heap_sys_mb"`
	HeapInuseMB float64 `json:"heap_inuse_mb"`
	SysMB       float64 `json:"sys_mb"`
	NumGC       uint32  `json:"num_gc"`
	Goroutines  int     `json:"goroutines"`
	// App
	UptimeSec   int64 `json:"uptime_sec"`
	ServerCount int   `json:"server_count"`
	OnlineCount int   `json:"online_count"`
	// SSH
	DialsOK      int64 `json:"dials_ok"`
	DialsFailed  int64 `json:"dials_failed"`
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`
	// HTTP
	ReqTotal  int64 `json:"req_total"`
	ReqActive int32 `json:"req_active"`
}

// Frame is the payload sent to each WebSocket client every 500ms.
type Frame struct {
	Stats   *Stats        `json:"stats,omitempty"`
	Logs    []LogEntry    `json:"logs,omitempty"`
	Timings []TimingEntry `json:"timings,omitempty"`
}

// Snapshot is the full state sent to a newly connected client.
type Snapshot struct {
	Stats   Stats         `json:"stats"`
	Logs    []LogEntry    `json:"logs"`
	Timings []TimingEntry `json:"timings"`
}

// ── Collector ─────────────────────────────────────────────────────────────────

type Collector struct {
	enabled   int32 // atomic 0|1
	startTime time.Time

	mu          sync.Mutex
	logs        []LogEntry
	timings     []TimingEntry
	serverCount int
	onlineCount int

	// Atomic counters — safe to increment from any goroutine
	dialsOK      int64
	dialsFailed  int64
	bytesRead    int64
	bytesWritten int64
	reqTotal     int64
	reqActive    int32

	subMu       sync.Mutex
	subscribers map[chan Frame]struct{}

	done chan struct{}
}

// Global is the process-wide debug collector.
var Global = &Collector{
	startTime:   time.Now(),
	subscribers: make(map[chan Frame]struct{}),
}

// ── Enable / Disable ──────────────────────────────────────────────────────────

func (c *Collector) Enabled() bool {
	return atomic.LoadInt32(&c.enabled) == 1
}

// Enable starts the collector. Returns false if already enabled.
func (c *Collector) Enable() bool {
	if !atomic.CompareAndSwapInt32(&c.enabled, 0, 1) {
		return false
	}
	c.done = make(chan struct{})
	go c.broadcastLoop()
	return true
}

// Disable stops the collector and closes all subscriber channels.
func (c *Collector) Disable() bool {
	if !atomic.CompareAndSwapInt32(&c.enabled, 1, 0) {
		return false
	}
	close(c.done)
	c.subMu.Lock()
	for ch := range c.subscribers {
		close(ch)
	}
	c.subscribers = make(map[chan Frame]struct{})
	c.subMu.Unlock()
	// Clear buffers to free memory
	c.mu.Lock()
	c.logs = nil
	c.timings = nil
	c.mu.Unlock()
	return true
}

// ── Subscribers ───────────────────────────────────────────────────────────────

func (c *Collector) Subscribe() chan Frame {
	ch := make(chan Frame, 8)
	c.subMu.Lock()
	c.subscribers[ch] = struct{}{}
	c.subMu.Unlock()
	return ch
}

func (c *Collector) Unsubscribe(ch chan Frame) {
	c.subMu.Lock()
	delete(c.subscribers, ch)
	c.subMu.Unlock()
}

// GetSnapshot returns full current state for a freshly connected client.
func (c *Collector) GetSnapshot() Snapshot {
	c.mu.Lock()
	logs := make([]LogEntry, len(c.logs))
	copy(logs, c.logs)
	timings := make([]TimingEntry, len(c.timings))
	copy(timings, c.timings)
	c.mu.Unlock()
	return Snapshot{Stats: c.buildStats(), Logs: logs, Timings: timings}
}

// ── Recording ─────────────────────────────────────────────────────────────────

func (c *Collector) Log(level Level, source, msg string) {
	if !c.Enabled() {
		return
	}
	e := LogEntry{T: time.Now().UnixMilli(), Level: level, Source: truncate(source), Msg: truncate(msg)}
	c.mu.Lock()
	c.logs = append(c.logs, e)
	if len(c.logs) > maxLogs {
		c.logs = c.logs[len(c.logs)-maxLogs:]
	}
	c.mu.Unlock()
}

func (c *Collector) RecordTiming(name string, d time.Duration) {
	if !c.Enabled() {
		return
	}
	e := TimingEntry{T: time.Now().UnixMilli(), Name: truncate(name), Ms: float64(d.Microseconds()) / 1000.0}
	c.mu.Lock()
	c.timings = append(c.timings, e)
	if len(c.timings) > maxTimings {
		c.timings = c.timings[len(c.timings)-maxTimings:]
	}
	c.mu.Unlock()
}

func (c *Collector) SetServerCounts(total, online int) {
	c.mu.Lock()
	c.serverCount = total
	c.onlineCount = online
	c.mu.Unlock()
}

// SSH counters
func (c *Collector) IncDialOK()              { atomic.AddInt64(&c.dialsOK, 1) }
func (c *Collector) IncDialFailed()          { atomic.AddInt64(&c.dialsFailed, 1) }
func (c *Collector) AddBytesRead(n int64)    { atomic.AddInt64(&c.bytesRead, n) }
func (c *Collector) AddBytesWritten(n int64) { atomic.AddInt64(&c.bytesWritten, n) }

// HTTP counters
func (c *Collector) IncRequest() {
	atomic.AddInt64(&c.reqTotal, 1)
	atomic.AddInt32(&c.reqActive, 1)
}
func (c *Collector) DecRequest() { atomic.AddInt32(&c.reqActive, -1) }

// ── Internal ──────────────────────────────────────────────────────────────────

func (c *Collector) buildStats() Stats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c.mu.Lock()
	sc, oc := c.serverCount, c.onlineCount
	c.mu.Unlock()
	return Stats{
		HeapAllocMB:  toMB(ms.HeapAlloc),
		HeapSysMB:    toMB(ms.HeapSys),
		HeapInuseMB:  toMB(ms.HeapInuse),
		SysMB:        toMB(ms.Sys),
		NumGC:        ms.NumGC,
		Goroutines:   runtime.NumGoroutine(),
		UptimeSec:    int64(time.Since(c.startTime).Seconds()),
		ServerCount:  sc,
		OnlineCount:  oc,
		DialsOK:      atomic.LoadInt64(&c.dialsOK),
		DialsFailed:  atomic.LoadInt64(&c.dialsFailed),
		BytesRead:    atomic.LoadInt64(&c.bytesRead),
		BytesWritten: atomic.LoadInt64(&c.bytesWritten),
		ReqTotal:     atomic.LoadInt64(&c.reqTotal),
		ReqActive:    atomic.LoadInt32(&c.reqActive),
	}
}

func (c *Collector) broadcastLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var logCursor, timCursor int

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			stats := c.buildStats()

			c.mu.Lock()
			// Guard against cursor being past end (can happen after trim)
			if logCursor > len(c.logs) {
				logCursor = 0
			}
			newLogs := make([]LogEntry, len(c.logs)-logCursor)
			copy(newLogs, c.logs[logCursor:])
			logCursor = len(c.logs)

			if timCursor > len(c.timings) {
				timCursor = 0
			}
			newTimings := make([]TimingEntry, len(c.timings)-timCursor)
			copy(newTimings, c.timings[timCursor:])
			timCursor = len(c.timings)
			c.mu.Unlock()

			frame := Frame{Stats: &stats, Logs: newLogs, Timings: newTimings}

			c.subMu.Lock()
			for ch := range c.subscribers {
				select {
				case ch <- frame:
				default: // drop frame if client is slow
				}
			}
			c.subMu.Unlock()
		}
	}
}

func toMB(b uint64) float64 { return float64(b) / 1024 / 1024 }

// ── Log interceptor ───────────────────────────────────────────────────────────

// NewLogInterceptor wraps dst so every write also feeds into the collector.
func NewLogInterceptor(dst io.Writer) io.Writer {
	return &logInterceptor{dst: dst}
}

type logInterceptor struct{ dst io.Writer }

func (l *logInterceptor) Write(p []byte) (int, error) {
	n, err := l.dst.Write(p)
	if Global.Enabled() {
		msg := strings.TrimRight(string(p), "\n\r")
		Global.Log(LevelInfo, "system", msg)
	}
	return n, err
}

// ── HTTP middleware ───────────────────────────────────────────────────────────

// Middleware wraps a handler to record timing and counts when debug is enabled.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Global.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		Global.IncRequest()
		start := time.Now()
		next.ServeHTTP(w, r)
		dur := time.Since(start)
		Global.DecRequest()
		name := r.Method + " " + r.URL.Path
		Global.RecordTiming(name, dur)
		Global.Log(LevelInfo, "http", fmt.Sprintf("%s  %.1fms", name, float64(dur.Microseconds())/1000))
	})
}
