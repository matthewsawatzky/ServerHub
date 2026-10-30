package ssh

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseStaticInfo(t *testing.T) {
	out := `boot_id=8c4e2f10-1234
hostname=web-1
cpu_model=Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz
cpu_threads=16
cpu_cores=8
os=Ubuntu 24.04.1 LTS
kernel=6.8.0-45-generic
arch=x86_64
mem_total=16384000
disk=/dev/sda1|/|52428800|104857600
disk=/dev/sdb1|/data|500|1000
disk=bad|line`
	info, err := parseStaticInfo(out)
	if err != nil {
		t.Fatalf("parseStaticInfo: %v", err)
	}
	if info.BootID != "8c4e2f10-1234" || info.Hostname != "web-1" {
		t.Fatalf("identity = %+v", info)
	}
	if info.CPUModel == "" || info.CPUCores != 8 || info.CPUThreads != 16 || info.OS != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("cpu/os = %+v", info)
	}
	if info.MemTotal != 16384000 || info.Kernel != "6.8.0-45-generic" || info.Arch != "x86_64" {
		t.Fatalf("mem/kernel = %+v", info)
	}
	if len(info.Disks) != 2 {
		t.Fatalf("disks = %+v", info.Disks)
	}
	if info.Disks[0].Percent != 50 || info.Disks[1].Percent != 50 {
		t.Errorf("disk percentages = %v, %v", info.Disks[0].Percent, info.Disks[1].Percent)
	}
}

func TestParseStaticInfoRejectsEmpty(t *testing.T) {
	if _, err := parseStaticInfo(""); err == nil {
		t.Fatal("expected an error for empty output")
	}
	if _, err := parseStaticInfo("garbage\nno equals here"); err == nil {
		t.Fatal("expected an error for non-key output")
	}
}

func TestCleanText(t *testing.T) {
	got := cleanText("  hello\x00\nworld\t!  ", 64)
	if got != "hello world !" {
		t.Fatalf("cleanText = %q", got)
	}
	long := cleanText("abcdef", 3)
	if long != "abc" {
		t.Fatalf("cap = %q", long)
	}
}

func TestParseCapabilities(t *testing.T) {
	out := `cap_tmux=1
cap_docker=1
cap_rsync=0
cap_sftp=1
cap_iperf3=0
cap_sudo_nopasswd=1`
	caps, err := parseCapabilities(out)
	if err != nil {
		t.Fatalf("parseCapabilities: %v", err)
	}
	if !caps.Tmux || !caps.Docker || caps.Rsync || !caps.SFTP || caps.Iperf3 || !caps.SudoNOPASSWD {
		t.Fatalf("caps = %+v", caps)
	}
	if _, err := parseCapabilities("nothing"); err == nil {
		t.Fatal("expected an error for empty capability output")
	}
}

func TestTaskBackoff(t *testing.T) {
	containers := &pollTask{interval: 30 * time.Second, maxBackoff: 10 * time.Minute, failures: 1}
	if got := taskBackoff(containers); got != 30*time.Second {
		t.Errorf("first failure = %v", got)
	}
	containers.failures = 3
	if got := taskBackoff(containers); got != 2*time.Minute {
		t.Errorf("third failure = %v", got)
	}
	static := &pollTask{interval: 6 * time.Hour, maxBackoff: time.Hour, failures: 1}
	if got := taskBackoff(static); got != time.Hour {
		t.Errorf("static cap = %v, want 1h", got)
	}
}

func TestJitterDuration(t *testing.T) {
	for _, d := range []time.Duration{5 * time.Second, 30 * time.Second, 6 * time.Hour} {
		got := jitterDuration(d)
		max := d / 10
		if max > 5*time.Minute {
			max = 5 * time.Minute
		}
		if got < d || got > d+max {
			t.Errorf("jitterDuration(%v) = %v", d, got)
		}
	}
	if jitterDuration(0) != 0 {
		t.Error("zero duration should not be jittered")
	}
}

func TestRunDueTasksRegistry(t *testing.T) {
	e := &ServerEntry{done: make(chan struct{})}
	e.stats.Online = true
	now := time.Now()

	var dueCalls, futureCalls, offlineCalls int
	e.tasks = []*pollTask{
		{
			name: "due", interval: time.Second, timeout: time.Second,
			next: now.Add(-time.Second),
			run: func(context.Context) (bool, time.Duration) {
				dueCalls++
				return true, 0
			},
		},
		{
			name: "future", interval: time.Hour, timeout: time.Second,
			next: now.Add(time.Hour),
			run: func(context.Context) (bool, time.Duration) {
				futureCalls++
				return true, 0
			},
		},
		{
			name: "offline", interval: time.Hour, timeout: time.Second, skipOffline: true,
			next: now.Add(-time.Second),
			run: func(context.Context) (bool, time.Duration) {
				offlineCalls++
				return true, 0
			},
		},
	}

	e.runDueTasks(now)
	if dueCalls != 1 || futureCalls != 0 || offlineCalls != 1 {
		t.Fatalf("calls = due:%d future:%d offline:%d", dueCalls, futureCalls, offlineCalls)
	}

	e.mu.Lock()
	e.stats.Online = false
	e.mu.Unlock()
	e.tasks[0].next = time.Now().Add(-time.Second)
	e.tasks[2].next = time.Now().Add(-time.Second)
	offlineCalls = 0
	e.runDueTasks(time.Now())
	if dueCalls != 2 || offlineCalls != 0 {
		t.Fatalf("offline skip: due=%d offline=%d", dueCalls, offlineCalls)
	}
	if wait := e.nextWake(); wait <= 0 || wait > 20*time.Second {
		t.Fatalf("nextWake = %v", wait)
	}
}

func TestGetInfoCopiesDisks(t *testing.T) {
	e := &ServerEntry{}
	e.static = StaticInfo{BootID: "boot", Disks: []DiskInfo{{Mount: "/"}}}
	e.caps = Capabilities{Tmux: true}

	info := e.GetInfo()
	if info.Static.BootID != "boot" || !info.Capabilities.Tmux {
		t.Fatalf("info = %+v", info)
	}
	info.Static.Disks[0].Mount = "/mutated"
	if e.static.Disks[0].Mount != "/" {
		t.Fatal("GetInfo returned the internal disk slice")
	}
}

func TestProbeShellSyntax(t *testing.T) {
	for name, script := range map[string]string{
		"static":       staticInfoCmd,
		"capabilities": capabilitiesCmd,
	} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script syntax: %v (%s)", name, err, out)
		}
	}
}

func TestProbeCommandsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("remote probe commands target Linux")
	}
	out, err := exec.Command("sh", "-c", staticInfoCmd).Output()
	if err != nil {
		t.Fatalf("static info command: %v", err)
	}
	info, err := parseStaticInfo(string(out))
	if err != nil {
		t.Fatalf("parse static info: %v\n%s", err, out)
	}
	if info.BootID == "" && info.OS == "" && info.CPUModel == "" {
		t.Fatalf("static probe returned no useful fields: %+v", info)
	}

	capsOut, err := exec.Command("sh", "-c", capabilitiesCmd).Output()
	if err != nil {
		t.Fatalf("capabilities command: %v", err)
	}
	if _, err := parseCapabilities(string(capsOut)); err != nil {
		t.Fatalf("parse capabilities: %v\n%s", err, capsOut)
	}
}
