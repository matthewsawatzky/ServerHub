package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu        sync.Mutex
	calls     []string
	streamCmd string
	streams   int
	runFn     func(cmd string) (string, error)

	// blockStreams keeps observe streams open until their context is done.
	blockStreams bool
	streamData   string
}

func (f *fakeRunner) RunCommand(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	fn := f.runFn
	f.mu.Unlock()
	if fn != nil {
		return fn(cmd)
	}
	return "", nil
}

func (f *fakeRunner) StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.streamCmd = cmd
	f.streams++
	block := f.blockStreams
	data := f.streamData
	f.mu.Unlock()

	if block {
		br := &blockReader{ch: make(chan struct{})}
		go func() {
			select {
			case <-ctx.Done():
				br.Close()
			case <-br.ch:
			}
		}()
		return br, nil
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRunner) lastCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeRunner) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

func (f *fakeRunner) observedCmd() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamCmd
}

func waitForStreams(t *testing.T, fr *fakeRunner, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for fr.streamCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d streams (have %d)", want, fr.streamCount())
		}
		time.Sleep(time.Millisecond)
	}
}

func readStreamUntilClosed(t *testing.T, ch <-chan []byte, timeout time.Duration) string {
	t.Helper()
	var out strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return out.String()
			}
			out.Write(chunk)
		case <-deadline:
			t.Fatal("timed out waiting for stream to close")
			return ""
		}
	}
}

type blockReader struct {
	ch   chan struct{}
	once sync.Once
}

func (b *blockReader) Read([]byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

func (b *blockReader) Close() error {
	b.once.Do(func() { close(b.ch) })
	return nil
}

func newTestRegistry(t *testing.T, path string) *Registry {
	t.Helper()
	r := NewRegistry(path)
	r.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	ids := []string{"job-one", "job-two", "job-three", "job-four", "job-five"}
	var mu sync.Mutex
	next := 0
	r.newID = func(string) string {
		mu.Lock()
		defer mu.Unlock()
		id := ids[next%len(ids)]
		next++
		return id
	}
	return r
}

func TestStartLaunchesJobAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	r := newTestRegistry(t, path)
	fr := &fakeRunner{}

	job, err := r.Start(context.Background(), fr, "srv", StartRequest{
		Command: "echo hi",
		Label:   "Hello",
		Source:  "manual",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.ID != "job-one" || job.Session != "serverhub-job-one" {
		t.Fatalf("unexpected job identity: %+v", job)
	}
	if job.Status != StatusRunning || job.StartedAt != 1_700_000_000 {
		t.Fatalf("unexpected job state: %+v", job)
	}

	call := fr.lastCall()
	for _, want := range []string{
		"tmux new-session -d -s",
		"tmux set-option -t",
		"remain-on-exit on",
		"tmux pipe-pane -t",
		"serverhub-job-one",
		"echo hi",
		"$HOME/.serverhub/jobs/job-one.log",
		"$HOME/.serverhub/jobs/job-one.status",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("launch command missing %q:\n%s", want, call)
		}
	}

	list := r.List("srv")
	if len(list) != 1 || list[0].ID != "job-one" {
		t.Fatalf("List = %+v", list)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("jobs.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("jobs.json mode = %o, want 600", perm)
	}

	loaded := NewRegistry(path)
	got, ok := loaded.Get("srv", "job-one")
	if !ok {
		t.Fatal("job did not round-trip through jobs.json")
	}
	if got.Command != "echo hi" || got.Status != StatusRunning {
		t.Fatalf("loaded job = %+v", got)
	}
}

func TestStartValidationAndSessionConflict(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{}

	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: " "}); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("empty command error = %v, want ErrInvalidJob", err)
	}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "true", Session: "Bad_ID"}); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("bad session error = %v, want ErrInvalidJob", err)
	}

	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "true", Session: "svc"}); err != nil {
		t.Fatalf("first fixed-session start: %v", err)
	}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "true", Session: "svc"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate fixed-session error = %v, want ErrSessionExists", err)
	}

	// A remote session unknown to the registry must also block a fixed name.
	r2 := newTestRegistry(t, "")
	fr2 := &fakeRunner{runFn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "has-session") {
			return "exists\n", nil
		}
		return "", nil
	}}
	if _, err := r2.Start(context.Background(), fr2, "srv", StartRequest{Command: "true", Session: "svc"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("remote session error = %v, want ErrSessionExists", err)
	}
}

func TestStopMarksJobStopped(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "sleep 100"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := r.Stop(context.Background(), fr, "srv", "job-one")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if job.Status != StatusStopped || job.FinishedAt != 1_700_000_000 {
		t.Fatalf("stopped job = %+v", job)
	}
	if call := fr.lastCall(); !strings.Contains(call, "tmux kill-session") || !strings.Contains(call, "serverhub-job-one") {
		t.Fatalf("stop command missing kill-session:\n%s", call)
	}
	if _, err := r.Stop(context.Background(), fr, "srv", "nope"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing job error = %v, want ErrJobNotFound", err)
	}
}

func TestRefreshUpdatesStatuses(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "two"}); err != nil {
		t.Fatal(err)
	}

	fr.runFn = func(cmd string) (string, error) {
		if strings.Contains(cmd, "for id in") {
			return "e job-one 7\ns job-two\n", nil
		}
		return "", nil
	}
	if err := r.Refresh(context.Background(), fr, "srv"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	one, _ := r.Get("srv", "job-one")
	if one.Status != StatusExited || one.ExitCode == nil || *one.ExitCode != 7 {
		t.Fatalf("job one = %+v", one)
	}
	two, _ := r.Get("srv", "job-two")
	if two.Status != StatusStopped {
		t.Fatalf("job two = %+v", two)
	}
}

func TestRefreshNotifiesOnChange(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{}
	for _, cmd := range []string{"one", "two"} {
		if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: cmd}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var events []string
	seven := 7
	r.SetOnChange(func(serverID, jobID, status string, exitCode *int) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, fmt.Sprintf("%s/%s=%s", serverID, jobID, status))
		if jobID == "job-one" {
			if exitCode == nil || *exitCode != seven {
				t.Errorf("job-one exit code = %v, want %d", exitCode, seven)
			}
		} else if exitCode != nil {
			t.Errorf("job-two should have no exit code, got %d", *exitCode)
		}
	})

	fr.runFn = func(cmd string) (string, error) {
		if strings.Contains(cmd, "for id in") {
			return "e job-one 7\ns job-two\n", nil
		}
		return "", nil
	}
	if err := r.Refresh(context.Background(), fr, "srv"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("change events = %+v, want 2", events)
	}
	sort.Strings(events)
	if events[0] != "srv/job-one=exited" || events[1] != "srv/job-two=stopped" {
		t.Fatalf("change events = %+v", events)
	}

	// A refresh with nothing new to report must not fire the hook again.
	fr.runFn = nil
	if err := r.Refresh(context.Background(), fr, "srv"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("change events after idle refresh = %+v", events)
	}
}

func TestDismissRemovesJob(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Dismiss(context.Background(), fr, "srv", "job-one"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if _, ok := r.Get("srv", "job-one"); ok {
		t.Fatal("job still present after dismiss")
	}
	if call := fr.lastCall(); !strings.Contains(call, "rm -f") {
		t.Fatalf("dismiss command missing cleanup:\n%s", call)
	}
	if err := r.Dismiss(context.Background(), fr, "srv", "job-one"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("second dismiss error = %v, want ErrJobNotFound", err)
	}
}

func TestSubscribeFansOutAndCapsObservers(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{blockStreams: true}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := r.Start(ctx, fr, "srv", StartRequest{Command: "sleep 100"}); err != nil {
			t.Fatal(err)
		}
	}

	ch1, close1, err := r.Subscribe(fr, "srv", "job-one")
	if err != nil {
		t.Fatalf("Subscribe job-one: %v", err)
	}
	ch2, close2, err := r.Subscribe(fr, "srv", "job-one")
	if err != nil {
		t.Fatalf("second Subscribe job-one: %v", err)
	}
	waitForStreams(t, fr, 1)
	if got := fr.streamCount(); got != 1 {
		t.Fatalf("stream count = %d, want 1 shared tail", got)
	}
	if !strings.Contains(fr.observedCmd(), "tail -n +1 -F") {
		t.Fatalf("observe command = %q", fr.observedCmd())
	}
	_ = ch1
	_ = ch2

	for _, id := range []string{"job-two", "job-three", "job-four"} {
		if _, closeFn, err := r.Subscribe(fr, "srv", id); err != nil {
			t.Fatalf("Subscribe %s: %v", id, err)
		} else {
			t.Cleanup(closeFn)
		}
	}
	waitForStreams(t, fr, 4)
	if got := fr.streamCount(); got != 4 {
		t.Fatalf("stream count = %d, want 4", got)
	}
	if _, _, err := r.Subscribe(fr, "srv", "job-five"); !errors.Is(err, ErrTooManyObservers) {
		t.Fatalf("fifth observer error = %v, want ErrTooManyObservers", err)
	}

	close1()
	close2()
	if _, closeFn, err := r.Subscribe(fr, "srv", "job-five"); err != nil {
		t.Fatalf("Subscribe after release: %v", err)
	} else {
		t.Cleanup(closeFn)
	}
}

func TestSubscribeStreamsOutput(t *testing.T) {
	r := newTestRegistry(t, "")
	fr := &fakeRunner{streamData: "hello\nworld\n"}
	if _, err := r.Start(context.Background(), fr, "srv", StartRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}

	ch, closeFn, err := r.Subscribe(fr, "srv", "job-one")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer closeFn()

	var out strings.Builder
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				if out.String() != "hello\nworld\n" {
					t.Fatalf("streamed output = %q", out.String())
				}
				return
			}
			out.Write(chunk)
		case <-deadline:
			t.Fatal("timed out waiting for stream to close")
		}
	}
}

// localRunner executes the remote shell scripts on this machine. Tests use a
// temporary HOME and TMUX_TMPDIR, so they exercise the real tmux/log/status
// flow without touching the user's sessions or ServerHub state.
type localRunner struct{ env []string }

func (l *localRunner) RunCommand(ctx context.Context, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Env = l.env
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (l *localRunner) StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Env = l.env
	pr, pw := io.Pipe()
	c.Stdout = pw
	c.Stderr = pw
	if err := c.Start(); err != nil {
		pw.Close()
		return nil, err
	}
	go func() {
		pw.CloseWithError(c.Wait())
	}()
	return pr, nil
}

func TestTmuxJobLifecycleLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tmux integration test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("tmux integration test requires a POSIX host")
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}

	home := t.TempDir()
	socketDir, err := os.MkdirTemp("/tmp", "serverhub-tmux-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) }) //nolint:errcheck — best-effort cleanup
	env := append(os.Environ(), "HOME="+home, "TMUX_TMPDIR="+socketDir)
	lr := &localRunner{env: env}
	t.Cleanup(func() {
		kill := exec.Command(tmux, "kill-server")
		kill.Env = env
		kill.Run() //nolint:errcheck — best-effort cleanup
	})

	r := NewRegistry("")
	r.newID = func(string) string { return "smoke-job" }

	job, err := r.Start(context.Background(), lr, "srv", StartRequest{
		Command: `printf 'hello from job\n'`,
		Label:   "Smoke",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.ID != "smoke-job" {
		t.Fatalf("job id = %q", job.ID)
	}

	statusFile := filepath.Join(home, ".serverhub", "jobs", "smoke-job.status")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(statusFile)
		if err == nil && strings.TrimSpace(string(data)) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not record exit status (err=%v, data=%q)", err, data)
		}
		time.Sleep(20 * time.Millisecond)
	}

	logFile := filepath.Join(home, ".serverhub", "jobs", "smoke-job.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading job log: %v", err)
	}
	if !strings.Contains(string(data), "hello from job") {
		t.Fatalf("job log = %q", data)
	}
	if info, err := os.Stat(logFile); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %o, want 600", info.Mode().Perm())
	}

	if err := r.Refresh(context.Background(), lr, "srv"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, _ := r.Get("srv", "smoke-job")
	if got.Status != StatusExited || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("refreshed job = %+v", got)
	}

	ch, unsubscribe, err := r.Subscribe(lr, "srv", "smoke-job")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	out := readStreamUntilClosed(t, ch, 5*time.Second)
	unsubscribe()
	if !strings.Contains(out, "hello from job") {
		t.Fatalf("observed output = %q", out)
	}

	if err := r.Dismiss(context.Background(), lr, "srv", "smoke-job"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	for _, path := range []string{logFile, statusFile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after dismiss", path)
		}
	}
}

func TestStartUsesCustomDir(t *testing.T) {
	runner := &fakeRunner{}
	reg := NewRegistry("")
	job, err := reg.Start(context.Background(), runner, "srv", StartRequest{
		Command: "true",
		Label:   "local",
		Dir:     "/tmp/serverhub-local-jobs",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Dir != "/tmp/serverhub-local-jobs" || !strings.HasPrefix(job.LogPath, "/tmp/serverhub-local-jobs/") {
		t.Fatalf("job paths = %+v", job)
	}
	if !strings.Contains(runner.lastCall(), "/tmp/serverhub-local-jobs") {
		t.Fatalf("launch script did not use custom dir: %s", runner.lastCall())
	}
}

func TestRefreshScriptUsesStatusPath(t *testing.T) {
	jobs := []Job{{
		ID:         "a",
		Dir:        "/tmp/x",
		StatusPath: "/tmp/x/a.status",
	}}
	script := refreshScript(jobs)
	if !strings.Contains(script, "/tmp/x/a.status") {
		t.Fatalf("refresh script missing status path: %s", script)
	}
	if strings.Contains(script, "$HOME/.serverhub/jobs") {
		t.Fatalf("refresh script still hardcodes HOME: %s", script)
	}
}
