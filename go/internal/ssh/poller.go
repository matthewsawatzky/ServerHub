package ssh

import (
	"context"
	"io"
	"log"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"serverhub/internal/debug"
)

const (
	ringSize             = 360 // 30 min at 5s
	pollInterval         = 5 * time.Second
	dockerInterval       = 30 * time.Second
	imageInterval        = 10 * time.Minute
	staticInterval       = 6 * time.Hour
	capabilitiesInterval = 6 * time.Hour
	offlineTaskRetry     = 15 * time.Second
	maxBackoff           = 60 * time.Second // stats reachability backoff

	statsPollTimeout        = 10 * time.Second
	dockerPollTimeout       = 30 * time.Second
	staticPollTimeout       = 15 * time.Second
	capabilitiesPollTimeout = 10 * time.Second
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
	images     ImageList
	static     StaticInfo
	caps       Capabilities
	done       chan struct{}
	wake       chan string
	stopOnce   sync.Once
	conn       *Conn
	imageMu    sync.Mutex // serialises polled and manual image refreshes

	// CPU counters from the previous poll, the current offline backoff, the
	// boot ID baseline, and the task registry. Only touched by this entry's
	// single polling goroutine.
	prevCPUTotal uint64
	prevCPUIdle  uint64
	failCount    int
	bootID       string
	tasks        []*pollTask
}

func NewServerEntry(id, host string, port int, user, keyPath string) *ServerEntry {
	return &ServerEntry{
		ID:      id,
		Host:    host,
		Port:    port,
		User:    user,
		KeyPath: keyPath,
		done:    make(chan struct{}),
		wake:    make(chan string, 4),
		conn:    NewConn(host, port, user, keyPath),
	}
}

// Stop signals the polling goroutine to exit and closes the shared control
// connection. Safe to call more than once.
func (e *ServerEntry) Stop() {
	e.stopOnce.Do(func() {
		close(e.done)
		e.conn.Close()
	})
}

// RunCommand runs a one-off command over this server's shared control
// connection. Cancelling ctx closes only that session, not the connection.
func (e *ServerEntry) RunCommand(ctx context.Context, cmd string) (string, error) {
	return e.conn.RunCommand(ctx, cmd)
}

// StreamCommand starts a streaming command over this server's shared control
// connection. The returned reader is closed when the caller closes it or ctx
// is cancelled.
func (e *ServerEntry) StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error) {
	return e.conn.StreamCommand(ctx, cmd)
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

// GetImages returns the cached Docker image list.
func (e *ServerEntry) GetImages() ImageList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	list := e.images
	list.Images = append([]Image{}, e.images.Images...)
	return list
}

// RefreshImages collects the image list now (manual refresh endpoint).
func (e *ServerEntry) RefreshImages(ctx context.Context) (ImageList, error) {
	return e.collectImages(ctx)
}

// RefreshContainers asks the poll goroutine to refresh the container list.
func (e *ServerEntry) RefreshContainers() { e.wakeTaskAsync("containers") }

// RefreshImagesAsync asks the poll goroutine to refresh the image list.
func (e *ServerEntry) RefreshImagesAsync() { e.wakeTaskAsync("images") }

func (e *ServerEntry) wakeTaskAsync(name string) {
	select {
	case e.wake <- name:
	default: // a wake is already pending; the task will run anyway
	}
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

// GetInfo returns a copy of the cached static host info and capabilities.
func (e *ServerEntry) GetInfo() ServerInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	info := ServerInfo{Static: e.static, Capabilities: e.caps}
	info.Static.Disks = append([]DiskInfo(nil), e.static.Disks...)
	return info
}

// pollTask is one entry in a host's poll registry. Tasks are owned by the
// single polling goroutine; no locks are needed for next/failures.
type pollTask struct {
	name        string
	interval    time.Duration
	timeout     time.Duration
	maxBackoff  time.Duration
	skipOffline bool
	next        time.Time
	failures    int
	run         func(context.Context) (ok bool, nextOverride time.Duration)
}

func (e *ServerEntry) initTasks() {
	now := time.Now()
	e.tasks = []*pollTask{
		{name: "stats", interval: pollInterval, timeout: statsPollTimeout, run: e.runStats, next: now},
		{name: "containers", interval: dockerInterval, timeout: dockerPollTimeout, maxBackoff: 10 * time.Minute, skipOffline: true, run: e.runContainers, next: now},
		{name: "images", interval: imageInterval, timeout: dockerPollTimeout, maxBackoff: time.Hour, skipOffline: true, run: e.runImages, next: now},
		{name: "static", interval: staticInterval, timeout: staticPollTimeout, maxBackoff: time.Hour, skipOffline: true, run: e.runStatic, next: now},
		{name: "capabilities", interval: capabilitiesInterval, timeout: capabilitiesPollTimeout, maxBackoff: time.Hour, skipOffline: true, run: e.runCapabilities, next: now},
	}
}

// StartOne begins the per-server poll registry in the background.
func StartOne(e *ServerEntry) {
	go e.pollLoop()
}

func (e *ServerEntry) pollLoop() {
	e.initTasks()
	for {
		e.runDueTasks(time.Now())
		wait := e.nextWake()
		timer := time.NewTimer(wait)
		select {
		case <-e.done:
			timer.Stop()
			return
		case <-timer.C:
		case name := <-e.wake:
			timer.Stop()
			e.wakeTask(name)
		}
	}
}

// runDueTasks runs every task whose deadline has passed, in registry order.
func (e *ServerEntry) runDueTasks(now time.Time) {
	for _, t := range e.tasks {
		if t.next.After(now) {
			continue
		}
		if t.skipOffline && !e.isOnline() {
			// Retry soon after the host comes back; stats owns the backoff.
			t.next = now.Add(offlineTaskRetry)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
		ok, override := t.run(ctx)
		cancel()
		if ok {
			t.failures = 0
			d := t.interval
			if override > 0 {
				d = override
			}
			t.next = time.Now().Add(jitterDuration(d))
			continue
		}
		t.failures++
		d := taskBackoff(t)
		if override > d {
			d = override
		}
		t.next = time.Now().Add(jitterDuration(d))
	}
}

func (e *ServerEntry) nextWake() time.Duration {
	var next time.Time
	for _, t := range e.tasks {
		if next.IsZero() || t.next.Before(next) {
			next = t.next
		}
	}
	if next.IsZero() {
		return time.Second
	}
	d := time.Until(next)
	if d < 0 {
		return 0
	}
	return d
}

func (e *ServerEntry) wakeTask(name string) {
	for _, t := range e.tasks {
		if t.name == name {
			t.next = time.Now()
		}
	}
}

func (e *ServerEntry) runStats(ctx context.Context) (bool, time.Duration) {
	start := time.Now()
	raw, err := e.conn.CollectStats(ctx)
	debug.Global.RecordTiming("ssh.stats."+e.ID, time.Since(start))

	e.mu.Lock()
	if err != nil {
		log.Printf("[%s] stats poll failed: %v", e.ID, err)
		e.stats.Online = false
		e.stats.UpdatedAt = time.Now().Unix()
		// Reset the CPU baseline: after an outage the next sample should not
		// report an average over the whole gap.
		e.prevCPUTotal, e.prevCPUIdle = 0, 0
		onUpdate := e.OnUpdate
		e.mu.Unlock()
		if onUpdate != nil {
			onUpdate()
		}
		return false, e.nextStatsDelay(false)
	}

	wasOnline := e.stats.Online
	firstBoot := e.bootID == "" && raw.BootID != ""
	bootChanged := e.bootID != "" && raw.BootID != "" && raw.BootID != e.bootID
	if raw.BootID != "" {
		e.bootID = raw.BootID
	}
	if bootChanged {
		// Static/capability data from the previous boot is stale.
		e.static = StaticInfo{}
		e.caps = Capabilities{}
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

	// CPU percentage from /proc/stat counters between consecutive polls.
	var cpuPct float64
	if e.prevCPUTotal > 0 && raw.CPUTotal > e.prevCPUTotal && raw.CPUIdle >= e.prevCPUIdle {
		dTotal := raw.CPUTotal - e.prevCPUTotal
		dIdle := raw.CPUIdle - e.prevCPUIdle
		cpuPct = math.Round(float64(dTotal-dIdle)/float64(dTotal)*1000) / 10
	}
	e.prevCPUTotal, e.prevCPUIdle = raw.CPUTotal, raw.CPUIdle

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
	if !wasOnline || firstBoot || bootChanged {
		e.wakeTask("containers")
		e.wakeTask("images")
		e.wakeTask("static")
		e.wakeTask("capabilities")
	}
	return true, e.nextStatsDelay(true)
}

func (e *ServerEntry) runContainers(ctx context.Context) (bool, time.Duration) {
	start := time.Now()
	containers, err := e.conn.CollectContainers(ctx)
	debug.Global.RecordTiming("ssh.docker."+e.ID, time.Since(start))

	e.mu.Lock()
	if err != nil {
		log.Printf("[%s] docker poll failed: %v", e.ID, err)
		e.mu.Unlock()
		return false, 0
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
	return true, 0
}

func (e *ServerEntry) runImages(ctx context.Context) (bool, time.Duration) {
	if _, err := e.collectImages(ctx); err != nil {
		log.Printf("[%s] image poll failed: %v", e.ID, err)
		return false, 0
	}
	return true, 0
}

func (e *ServerEntry) collectImages(ctx context.Context) (ImageList, error) {
	start := time.Now()
	e.imageMu.Lock()
	defer e.imageMu.Unlock()
	images, err := e.conn.CollectImages(ctx)
	debug.Global.RecordTiming("ssh.images."+e.ID, time.Since(start))
	if err != nil {
		return ImageList{}, err
	}
	if images == nil {
		images = []Image{}
	}
	list := ImageList{UpdatedAt: time.Now().Unix(), Images: images}
	e.mu.Lock()
	e.images = list
	e.mu.Unlock()
	return list, nil
}

func (e *ServerEntry) runStatic(ctx context.Context) (bool, time.Duration) {
	start := time.Now()
	out, err := e.conn.RunCommand(ctx, staticInfoCmd)
	debug.Global.RecordTiming("ssh.static."+e.ID, time.Since(start))
	if err != nil {
		log.Printf("[%s] static info poll failed: %v", e.ID, err)
		return false, 0
	}
	info, err := parseStaticInfo(out)
	if err != nil {
		log.Printf("[%s] static info parse failed: %v", e.ID, err)
		return false, 0
	}
	info.UpdatedAt = time.Now().Unix()
	e.mu.Lock()
	e.static = info
	e.mu.Unlock()
	return true, 0
}

func (e *ServerEntry) runCapabilities(ctx context.Context) (bool, time.Duration) {
	start := time.Now()
	out, err := e.conn.RunCommand(ctx, capabilitiesCmd)
	debug.Global.RecordTiming("ssh.caps."+e.ID, time.Since(start))
	if err != nil {
		log.Printf("[%s] capability poll failed: %v", e.ID, err)
		return false, 0
	}
	caps, err := parseCapabilities(out)
	if err != nil {
		log.Printf("[%s] capability parse failed: %v", e.ID, err)
		return false, 0
	}
	caps.UpdatedAt = time.Now().Unix()
	e.mu.Lock()
	e.caps = caps
	e.mu.Unlock()
	return true, 0
}

// isOnline reports the last known reachability without touching the network.
func (e *ServerEntry) isOnline() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stats.Online
}

// nextStatsDelay grows the polling interval while a host is unreachable so a
// dead box is not dialled every 5 seconds forever.
func (e *ServerEntry) nextStatsDelay(online bool) time.Duration {
	if online {
		e.failCount = 0
		return pollInterval
	}
	if e.failCount < 6 {
		e.failCount++
	}
	d := pollInterval * time.Duration(1<<e.failCount)
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func taskBackoff(t *pollTask) time.Duration {
	if t.failures < 1 {
		t.failures = 1
	}
	d := t.interval
	if t.maxBackoff > 0 && d > t.maxBackoff {
		d = t.maxBackoff
	}
	for i := 1; i < t.failures; i++ {
		if t.maxBackoff > 0 && d >= t.maxBackoff/2 {
			d = t.maxBackoff
			break
		}
		d *= 2
		if t.maxBackoff > 0 && d > t.maxBackoff {
			d = t.maxBackoff
			break
		}
	}
	if d <= 0 {
		d = time.Minute
	}
	return d
}

// jitterDuration adds up to 10% (capped at five minutes) so a fleet does not
// poll or schedule in lockstep.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	max := d / 10
	if max > 5*time.Minute {
		max = 5 * time.Minute
	}
	if max <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(max)+1))
}

func StartPolling(entries []*ServerEntry) {
	for _, entry := range entries {
		StartOne(entry)
	}
}
