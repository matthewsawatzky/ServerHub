package ssh

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DiskInfo is one mounted filesystem from `df`.
type DiskInfo struct {
	Filesystem string  `json:"filesystem"`
	Mount      string  `json:"mount"`
	Used       int64   `json:"used"`
	Total      int64   `json:"total"`
	Percent    float64 `json:"percent"`
}

// StaticInfo is the slowly-changing host inventory fetched on connect and
// every few hours (or when the boot ID changes).
type StaticInfo struct {
	BootID     string     `json:"boot_id,omitempty"`
	Hostname   string     `json:"hostname,omitempty"`
	CPUModel   string     `json:"cpu_model,omitempty"`
	CPUCores   int        `json:"cpu_cores,omitempty"`   // physical cores
	CPUThreads int        `json:"cpu_threads,omitempty"` // logical processors / vCPUs
	OS         string     `json:"os,omitempty"`
	Kernel     string     `json:"kernel,omitempty"`
	Arch       string     `json:"arch,omitempty"`
	MemTotal   int64      `json:"mem_total,omitempty"` // kB, same unit as ServerStats
	Disks      []DiskInfo `json:"disks,omitempty"`
	UpdatedAt  int64      `json:"updated_at,omitempty"`
}

// Capabilities is the remote tool inventory. `sudo -n` is checked because
// scheduled apt-style actions should fail fast without a NOPASSWD rule.
type Capabilities struct {
	Tmux         bool  `json:"tmux"`
	Docker       bool  `json:"docker"`
	Rsync        bool  `json:"rsync"`
	SFTP         bool  `json:"sftp"`
	Iperf3       bool  `json:"iperf3"`
	SudoNOPASSWD bool  `json:"sudo_nopasswd"`
	UpdatedAt    int64 `json:"updated_at,omitempty"`
}

// ServerInfo is the expanded detail payload (deliberately not in the SSE card).
type ServerInfo struct {
	Static       StaticInfo   `json:"static"`
	Capabilities Capabilities `json:"capabilities"`
}

const staticInfoCmd = `boot_id=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)
echo "boot_id=$boot_id"
echo "hostname=$(hostname 2>/dev/null)"
echo "cpu_model=$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null)"
cpu_threads=$(nproc 2>/dev/null || grep -c '^processor' /proc/cpuinfo 2>/dev/null)
cpu_cores=$(awk -F': ' '/^processor[[:space:]]*:/{p=""} /^physical id[[:space:]]*:/{p=$2} /^core id[[:space:]]*:/{if (p != "") print p ":" $2}' /proc/cpuinfo 2>/dev/null | sort -u | wc -l | tr -d ' ')
if [ -z "$cpu_cores" ] || [ "$cpu_cores" = "0" ]; then cpu_cores=$cpu_threads; fi
echo "cpu_threads=$cpu_threads"
echo "cpu_cores=$cpu_cores"
echo "os=$(grep '^PRETTY_NAME=' /etc/os-release 2>/dev/null | head -n1 | cut -d= -f2- | tr -d '\"')"
echo "kernel=$(uname -r 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "mem_total=$(awk '/^MemTotal:/{print $2; exit}' /proc/meminfo 2>/dev/null)"
df -P -B1 --output=source,target,used,size -x tmpfs -x devtmpfs -x squashfs 2>/dev/null | tail -n +2 | while read -r fs mount used total; do
  [ -n "$mount" ] && echo "disk=$fs|$mount|$used|$total"
done`

const capabilitiesCmd = `for c in tmux docker rsync iperf3; do
  if command -v "$c" >/dev/null 2>&1; then echo "cap_$c=1"; else echo "cap_$c=0"; fi
done
if command -v sftp-server >/dev/null 2>&1 || [ -x /usr/lib/openssh/sftp-server ]; then echo "cap_sftp=1"; else echo "cap_sftp=0"; fi
if sudo -n true >/dev/null 2>&1; then echo "cap_sudo_nopasswd=1"; else echo "cap_sudo_nopasswd=0"; fi`

func parseStaticInfo(out string) (StaticInfo, error) {
	var info StaticInfo
	seen := false
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "boot_id":
			info.BootID = cleanText(val, 128)
			seen = true
		case "hostname":
			info.Hostname = cleanText(val, 255)
			seen = true
		case "cpu_model":
			info.CPUModel = cleanText(val, 255)
			seen = true
		case "cpu_threads":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				info.CPUThreads = n
			}
			seen = true
		case "cpu_cores":
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && n > 0 {
				info.CPUCores = n
			}
			seen = true
		case "os":
			info.OS = cleanText(val, 255)
			seen = true
		case "kernel":
			info.Kernel = cleanText(val, 128)
			seen = true
		case "arch":
			info.Arch = cleanText(val, 32)
			seen = true
		case "mem_total":
			if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && n > 0 {
				info.MemTotal = n
			}
			seen = true
		case "disk":
			if d, ok := parseDisk(val); ok && len(info.Disks) < 20 {
				info.Disks = append(info.Disks, d)
			}
			seen = true
		}
	}
	if !seen {
		return info, fmt.Errorf("no static info in output")
	}
	return info, nil
}

func parseDisk(val string) (DiskInfo, bool) {
	parts := strings.Split(val, "|")
	if len(parts) != 4 {
		return DiskInfo{}, false
	}
	used, err1 := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	total, err2 := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
	if err1 != nil || err2 != nil || used < 0 || total <= 0 || used > total {
		return DiskInfo{}, false
	}
	d := DiskInfo{
		Filesystem: cleanText(parts[0], 128),
		Mount:      cleanText(parts[1], 255),
		Used:       used,
		Total:      total,
	}
	d.Percent = math.Round(float64(used)/float64(total)*1000) / 10
	if d.Filesystem == "" || d.Mount == "" {
		return DiskInfo{}, false
	}
	return d, true
}

func parseCapabilities(out string) (Capabilities, error) {
	var c Capabilities
	seen := false
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		on := strings.TrimSpace(val) == "1"
		switch key {
		case "cap_tmux":
			c.Tmux = on
		case "cap_docker":
			c.Docker = on
		case "cap_rsync":
			c.Rsync = on
		case "cap_sftp":
			c.SFTP = on
		case "cap_iperf3":
			c.Iperf3 = on
		case "cap_sudo_nopasswd":
			c.SudoNOPASSWD = on
		default:
			continue
		}
		seen = true
	}
	if !seen {
		return c, fmt.Errorf("no capability info in output")
	}
	return c, nil
}

// cleanText strips control characters and bounds remote-controlled strings
// before they are stored or sent to the browser.
func cleanText(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
