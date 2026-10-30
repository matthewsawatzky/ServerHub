package ssh

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"serverhub/internal/debug"
	"serverhub/internal/storage"
)

const dialTimeout = 3 * time.Second

// ── Host key policy (TOFU) ────────────────────────────────────────────────────

var (
	policyMu    sync.Mutex
	insecureKey bool       // insecure_ignore_host_key config flag
	tofuFile    string     // path to known_hosts
	tofuMu      sync.Mutex // serialises known_hosts reads + appends
)

// SetHostKeyPolicy configures SSH host key verification. It must be called
// once at startup before any dial. With insecure=false (default) connections
// use trust-on-first-use against knownHostsPath: the first connection records
// the host key, later connections verify it and fail on change.
func SetHostKeyPolicy(insecure bool, knownHostsPath string) {
	policyMu.Lock()
	defer policyMu.Unlock()
	insecureKey = insecure
	tofuFile = knownHostsPath
	if insecure {
		log.Printf("[ssh] WARNING: insecure_ignore_host_key is enabled — SSH host keys are not verified (connections are MITM-able)")
	}
}

func hostKeyCallback() (gossh.HostKeyCallback, error) {
	policyMu.Lock()
	defer policyMu.Unlock()
	if insecureKey {
		return gossh.InsecureIgnoreHostKey(), nil //nolint:gosec — explicitly opt-in via config
	}
	if tofuFile == "" {
		return nil, fmt.Errorf("host key verification is enabled but no known_hosts path is configured")
	}
	if err := ensureKnownHostsFileLocked(); err != nil {
		return nil, err
	}
	verify, err := knownhosts.New(tofuFile)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts: %w", err)
	}
	return func(host string, remote net.Addr, key gossh.PublicKey) error {
		err := verify(host, remote, key)
		if err == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if !errors.As(err, &kerr) {
			return err
		}
		if kerr.Want != nil {
			// Known host, different key — refuse loudly.
			return fmt.Errorf(
				"host key mismatch for %s — the server's key changed or the connection is being intercepted; verify the new key out-of-band (e.g. with ssh) and remove the stale entry from %s",
				host, tofuFile)
		}
		// Unknown host — trust on first use: record and continue.
		if err := recordKnownHost(remote, key); err != nil {
			return fmt.Errorf("recording first host key: %w", err)
		}
		log.Printf("[ssh] recorded host key for %s (first connection) in %s", host, tofuFile)
		return nil
	}, nil
}

// ensureKnownHostsFile creates the known_hosts file if missing so the
// checker always parses an existing file.
func ensureKnownHostsFileLocked() error {
	tofuMu.Lock()
	defer tofuMu.Unlock()
	return storage.EnsureFile(tofuFile)
}

// recordKnownHost appends the host key if not already recorded (TOFU).
func recordKnownHost(remote net.Addr, key gossh.PublicKey) error {
	tofuMu.Lock()
	defer tofuMu.Unlock()
	line := knownhosts.Normalize(remote.String()) + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
	// Another concurrent dial may have recorded it already.
	if data, err := os.ReadFile(tofuFile); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) == line {
				return nil
			}
		}
	}
	if err := storage.Append(tofuFile, line); err != nil {
		return err
	}
	return nil
}

type RawStats struct {
	Uptime   int64  `json:"uptime"`
	CPUTotal uint64 `json:"cpu_total"`
	CPUIdle  uint64 `json:"cpu_idle"`
	MemTotal int64  `json:"mem_total"`
	MemAvail int64  `json:"mem_avail"`
	Disk     string `json:"disk"`
	Load     string `json:"load"`
	BootID   string `json:"boot_id"`
}

type Container struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  string `json:"state"`
}

// statsCmd returns raw kernel counters; CPU percentage is derived by the
// poller from consecutive samples (same approach as the local collector), so
// there is no dependency on top/mpstat and no fixed sampling window.
const statsCmd = `uptime_sec=$(awk '{print int($1)}' /proc/uptime); ` +
	`cpu=$(awk '/^cpu /{t=0; for(i=2;i<=NF;i++) t+=$i; print t, $5+$6; exit}' /proc/stat); ` +
	`set -- $cpu; cpu_total=${1:-0}; cpu_idle=${2:-0}; ` +
	`mem_total=$(grep MemTotal /proc/meminfo | awk '{print $2}'); ` +
	`mem_avail=$(grep MemAvailable /proc/meminfo | awk '{print $2}'); ` +
	`disk=$(df / --output=used,size 2>/dev/null | tail -1 | tr -s ' '); ` +
	`load=$(cat /proc/loadavg); ` +
	`boot_id=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null); ` +
	`echo "{\"uptime\":$uptime_sec,\"cpu_total\":${cpu_total:-0},\"cpu_idle\":${cpu_idle:-0},\"mem_total\":$mem_total,\"mem_avail\":$mem_avail,\"disk\":\"$disk\",\"load\":\"$load\",\"boot_id\":\"$boot_id\"}"`

const dockerCmd = `docker ps -a --format '{"name":"{{.Names}}","status":"{{.Status}}","state":"{{.State}}"}'`

func loadKey(path string) (gossh.AuthMethod, error) {
	if path == "" {
		return nil, fmt.Errorf("no key path configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key %s: %w", path, err)
	}
	signer, err := gossh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing key: %w", err)
	}
	return gossh.PublicKeys(signer), nil
}

// countingConn wraps a net.Conn and feeds byte counts into the debug collector.
type countingConn struct{ net.Conn }

func (c *countingConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		debug.Global.AddBytesRead(int64(n))
	}
	return
}

func (c *countingConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		debug.Global.AddBytesWritten(int64(n))
	}
	return
}

func Dial(host string, port int, user, keyPath string) (*gossh.Client, error) {
	auth, err := loadKey(keyPath)
	if err != nil {
		return nil, err
	}
	hkc, err := hostKeyCallback()
	if err != nil {
		return nil, err
	}
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{auth},
		HostKeyCallback: hkc,
		Timeout:         dialTimeout,
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	raw, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		debug.Global.IncDialFailed()
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	conn := &countingConn{Conn: raw}
	sshConn, chans, reqs, err := gossh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		debug.Global.IncDialFailed()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	debug.Global.IncDialOK()
	return gossh.NewClient(sshConn, chans, reqs), nil
}

// containerNamePattern is Docker's own charset for container names
// ([a-zA-Z0-9][a-zA-Z0-9_.-]*). Anything else a remote host reports is
// attacker-controlled text — dropped at the boundary (XSS defence in depth).
var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// SanitizeContainer drops containers whose name deviates from the expected
// Docker charset. Status/state are never interpolated into trusted contexts
// client-side, so only the name needs this gate.
func SanitizeContainer(c Container) bool {
	return c.Name != "" && len(c.Name) <= 128 && containerNamePattern.MatchString(c.Name)
}
