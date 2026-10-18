// Package local collects system stats from the machine running ServerHub.
// It mirrors the ServerStats / StatPoint types from the ssh package so the
// same rendering path works for both remote and local cards.
package local

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	sshclient "serverhub/internal/ssh"
)

const (
	ringSize     = 360 // 30 min at 5 s intervals
	pollInterval = 5 * time.Second
)

// Collector gathers stats from the local machine on a fixed ticker.
type Collector struct {
	hostname string

	mu        sync.RWMutex
	stats     sshclient.ServerStats
	ring      [ringSize]sshclient.StatPoint
	ringHead  int
	ringCount int
	onUpdate  func() // called after each collect; set via SetOnUpdate

	// prevCPU* is only touched by the single polling goroutine — no lock needed.
	prevCPUTotal uint64
	prevCPUIdle  uint64
}

// SetOnUpdate registers a callback invoked after every stats collection.
func (c *Collector) SetOnUpdate(f func()) {
	c.mu.Lock()
	c.onUpdate = f
	c.mu.Unlock()
}

// New returns an initialised Collector (does not start polling yet).
func New() *Collector {
	hostname, _ := os.Hostname()
	return &Collector{hostname: hostname}
}

// Hostname returns the local machine's hostname.
func (c *Collector) Hostname() string { return c.hostname }

// Start begins background polling. Call once.
func (c *Collector) Start() {
	go func() {
		c.collect()
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for range t.C {
			c.collect()
		}
	}()
}

// GetStats returns a snapshot of the latest stats plus sparkline history.
func (c *Collector) GetStats() sshclient.ServerStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.stats
	s.History = c.getHistory()
	return s
}

func (c *Collector) getHistory() []sshclient.StatPoint {
	if c.ringCount == 0 {
		return []sshclient.StatPoint{}
	}
	out := make([]sshclient.StatPoint, c.ringCount)
	start := (c.ringHead - c.ringCount + ringSize) % ringSize
	for i := 0; i < c.ringCount; i++ {
		out[i] = c.ring[(start+i)%ringSize]
	}
	return out
}

// ── internal ──────────────────────────────────────────────────────────────────

type rawStats struct {
	uptime    int64
	cpu       float64
	memTotal  int64 // KB
	memAvail  int64 // KB
	diskUsed  int64 // KB
	diskTotal int64 // KB
	load1     string
	load5     string
	load15    string
}

func (c *Collector) collect() {
	var raw rawStats
	var err error

	switch runtime.GOOS {
	case "linux":
		raw, err = c.collectLinux()
	case "darwin":
		raw, err = collectDarwin()
	default:
		raw, err = collectGeneric()
	}

	c.mu.Lock()

	if err != nil {
		c.stats.Online = false
		c.stats.UpdatedAt = time.Now().Unix()
		onUpdate := c.onUpdate
		c.mu.Unlock()
		if onUpdate != nil {
			onUpdate()
		}
		return
	}

	memUsed := raw.memTotal - raw.memAvail
	if memUsed < 0 {
		memUsed = 0
	}
	var memPct float64
	if raw.memTotal > 0 {
		memPct = math.Round(float64(memUsed)/float64(raw.memTotal)*1000) / 10
	}
	var diskPct float64
	if raw.diskTotal > 0 {
		diskPct = math.Round(float64(raw.diskUsed)/float64(raw.diskTotal)*1000) / 10
	}

	c.stats = sshclient.ServerStats{
		Online:    true,
		UpdatedAt: time.Now().Unix(),
		Uptime:    raw.uptime,
		CPU:       raw.cpu,
		MemTotal:  raw.memTotal,
		MemUsed:   memUsed,
		MemPct:    memPct,
		DiskUsed:  raw.diskUsed,
		DiskTotal: raw.diskTotal,
		DiskPct:   diskPct,
		Load1:     raw.load1,
		Load5:     raw.load5,
		Load15:    raw.load15,
	}

	pt := sshclient.StatPoint{
		Time:    time.Now().Unix(),
		CPU:     raw.cpu,
		MemPct:  memPct,
		DiskPct: diskPct,
	}
	c.ring[c.ringHead] = pt
	c.ringHead = (c.ringHead + 1) % ringSize
	if c.ringCount < ringSize {
		c.ringCount++
	}
	onUpdate := c.onUpdate
	c.mu.Unlock()
	if onUpdate != nil {
		onUpdate()
	}
}

// ── Linux ─────────────────────────────────────────────────────────────────────

type cpuSnapshot struct {
	total uint64
	idle  uint64
}

func readLinuxCPUStat() (cpuSnapshot, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSnapshot{}, err
	}
	for _, line := range strings.SplitN(string(data), "\n", 10) {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop "cpu" label
		var vals []uint64
		for _, f := range fields {
			v, _ := strconv.ParseUint(f, 10, 64)
			vals = append(vals, v)
		}
		if len(vals) < 4 {
			break
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4] // include iowait in "idle" for display
		}
		return cpuSnapshot{total: total, idle: idle}, nil
	}
	return cpuSnapshot{}, fmt.Errorf("cpu line not found in /proc/stat")
}

func (c *Collector) collectLinux() (rawStats, error) {
	var s rawStats

	// Uptime
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if parts := strings.Fields(string(data)); len(parts) > 0 {
			f, _ := strconv.ParseFloat(parts[0], 64)
			s.uptime = int64(f)
		}
	}

	// Memory
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "MemTotal:") {
				if fields := strings.Fields(line); len(fields) >= 2 {
					s.memTotal, _ = strconv.ParseInt(fields[1], 10, 64)
				}
			} else if strings.HasPrefix(line, "MemAvailable:") {
				if fields := strings.Fields(line); len(fields) >= 2 {
					s.memAvail, _ = strconv.ParseInt(fields[1], 10, 64)
				}
			}
		}
	}

	// Load average
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) >= 3 {
			s.load1 = fields[0]
			s.load5 = fields[1]
			s.load15 = fields[2]
		}
	}

	// CPU: delta from previous reading (single polling goroutine, no lock needed)
	if snap, err := readLinuxCPUStat(); err == nil {
		if c.prevCPUTotal > 0 && snap.total > c.prevCPUTotal {
			dTotal := snap.total - c.prevCPUTotal
			dIdle := snap.idle - c.prevCPUIdle
			if dTotal > 0 {
				s.cpu = math.Round((float64(dTotal-dIdle)/float64(dTotal))*1000) / 10
			}
		}
		c.prevCPUTotal = snap.total
		c.prevCPUIdle = snap.idle
	}

	// Disk
	if out, err := exec.Command("df", "-k", "/").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			if fields := strings.Fields(lines[len(lines)-1]); len(fields) >= 3 {
				s.diskTotal, _ = strconv.ParseInt(fields[1], 10, 64)
				s.diskUsed, _ = strconv.ParseInt(fields[2], 10, 64)
			}
		}
	}

	return s, nil
}

// ── Darwin ────────────────────────────────────────────────────────────────────

func collectDarwin() (rawStats, error) {
	var s rawStats

	// Uptime via sysctl kern.boottime → "{ sec = XXXX, usec = YYYY }"
	if out, err := exec.Command("sysctl", "-n", "kern.boottime").Output(); err == nil {
		str := string(out)
		if idx := strings.Index(str, "sec = "); idx >= 0 {
			rest := str[idx+6:]
			end := strings.IndexAny(rest, ", }")
			if end < 0 {
				end = len(rest)
			}
			if sec, err := strconv.ParseInt(strings.TrimSpace(rest[:end]), 10, 64); err == nil && sec > 0 {
				s.uptime = time.Now().Unix() - sec
			}
		}
	}

	// Load average via sysctl vm.loadavg → "{ 1.23 4.56 7.89 }"
	if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		str := strings.Trim(strings.TrimSpace(string(out)), "{} ")
		if fields := strings.Fields(str); len(fields) >= 3 {
			s.load1 = fields[0]
			s.load5 = fields[1]
			s.load15 = fields[2]
		}
	}

	// Memory total via hw.memsize (bytes → KB)
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			s.memTotal = total / 1024
		}
	}

	// Memory used via vm_stat (page counts × page size)
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		var pageSize int64 = 4096
		if psOut, psErr := exec.Command("pagesize").Output(); psErr == nil {
			pageSize, _ = strconv.ParseInt(strings.TrimSpace(string(psOut)), 10, 64)
		}
		var active, wired, compressed int64
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			line := sc.Text()
			parseVMStatLine(line, "Pages active:", &active)
			parseVMStatLine(line, "Pages wired down:", &wired)
			parseVMStatLine(line, "Pages occupied by compressor:", &compressed)
		}
		usedKB := (active + wired + compressed) * pageSize / 1024
		s.memAvail = s.memTotal - usedKB
		if s.memAvail < 0 {
			s.memAvail = 0
		}
	}

	// CPU via top -l 1 -n 0 → "CPU usage: 3.66% user, 5.91% sys, 90.41% idle"
	if out, err := exec.Command("top", "-l", "1", "-n", "0").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			lower := strings.ToLower(line)
			if !strings.Contains(lower, "cpu") || !strings.Contains(lower, "idle") {
				continue
			}
			if idx := strings.Index(lower, "idle"); idx >= 0 {
				before := strings.TrimSpace(line[:idx])
				parts := strings.Fields(before)
				if len(parts) > 0 {
					idleStr := strings.TrimSuffix(parts[len(parts)-1], "%")
					if idlePct, err := strconv.ParseFloat(idleStr, 64); err == nil {
						s.cpu = math.Round((100.0-idlePct)*10) / 10
					}
				}
			}
			break
		}
	}

	// Disk
	if out, err := exec.Command("df", "-k", "/").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			if fields := strings.Fields(lines[len(lines)-1]); len(fields) >= 3 {
				s.diskTotal, _ = strconv.ParseInt(fields[1], 10, 64)
				s.diskUsed, _ = strconv.ParseInt(fields[2], 10, 64)
			}
		}
	}

	return s, nil
}

func parseVMStatLine(line, prefix string, out *int64) {
	if !strings.HasPrefix(line, prefix) {
		return
	}
	rest := strings.TrimRight(strings.TrimSpace(strings.TrimPrefix(line, prefix)), ".")
	*out, _ = strconv.ParseInt(rest, 10, 64)
}

// ── Generic stub ──────────────────────────────────────────────────────────────

func collectGeneric() (rawStats, error) {
	return rawStats{}, nil
}
