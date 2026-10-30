// Package jobs manages detached remote jobs that run under tmux on a host.
// Jobs survive ServerHub restarts and disconnects; the remote tmux session and
// the job's log/status files are the source of truth, while jobs.json is a
// rebuildable cache.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"serverhub/internal/config"
	"serverhub/internal/storage"
)

const (
	StatusRunning = "running"
	StatusExited  = "exited"
	StatusStopped = "stopped"
)

const (
	remoteJobsDir    = "$HOME/.serverhub/jobs"
	maxCommandLength = 8192
	maxLabelLength   = 128
	maxObservers     = 4
)

var (
	ErrJobNotFound      = errors.New("job not found")
	ErrInvalidJob       = errors.New("invalid job")
	ErrSessionExists    = errors.New("tmux session already exists")
	ErrTooManyObservers = errors.New("too many observed jobs on this host")
)

// Runner is the per-host control connection the registry uses to launch,
// inspect, and stream jobs.
type Runner interface {
	RunCommand(ctx context.Context, cmd string) (string, error)
	StreamCommand(ctx context.Context, cmd string) (io.ReadCloser, error)
}

// Job is one detached tmux job on a host.
type Job struct {
	ID         string `json:"id"`
	ServerID   string `json:"server_id"`
	Label      string `json:"label,omitempty"`
	Command    string `json:"command,omitempty"`
	Session    string `json:"session"`
	Dir        string `json:"-"`
	LogPath    string `json:"log_path"`
	StatusPath string `json:"status_path"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Source     string `json:"source,omitempty"`
	ScheduleID string `json:"schedule_id,omitempty"`
}

// StartRequest describes a new job. Session is optional: when set it is used
// verbatim as the job ID (after validation), which suits service-like actions;
// otherwise an ID is derived from the label plus a date/random suffix. Dir is
// optional; the local machine uses it to keep job state inside storage.Dir().
type StartRequest struct {
	Command    string `json:"command"`
	Label      string `json:"label"`
	Session    string `json:"session"`
	Source     string `json:"source"`
	ScheduleID string `json:"schedule_id,omitempty"`
	Dir        string `json:"-"`
}

type jobsFile struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

type Registry struct {
	startMu sync.Mutex // serialises remote session-conflict checks + launches

	mu             sync.Mutex
	path           string
	jobs           map[string]*Job // key: serverID + "\x00" + job ID
	streams        map[string]*stream
	observerCounts map[string]int // active observe streams per server
	onChange       func(serverID, jobID, status string, exitCode *int)

	now   func() time.Time
	newID func(label string) string
}

func jobKey(serverID, id string) string { return serverID + "\x00" + id }

// NewRegistry loads the job cache from path (may be empty for tests). A
// missing or corrupt file yields an empty registry; a corrupt file is logged
// loudly but never blocks startup.
func NewRegistry(path string) *Registry {
	r := &Registry{
		path:           path,
		jobs:           make(map[string]*Job),
		streams:        make(map[string]*stream),
		observerCounts: make(map[string]int),
		now:            time.Now,
		newID:          newJobID,
	}
	if path == "" {
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("[jobs] could not read %s: %v", path, err)
		}
		return r
	}
	var file jobsFile
	if err := json.Unmarshal(data, &file); err != nil {
		log.Printf("[jobs] corrupt %s ignored: %v", path, err)
		return r
	}
	for _, j := range file.Jobs {
		if j.ID == "" || j.ServerID == "" {
			continue
		}
		job := j
		r.jobs[jobKey(job.ServerID, job.ID)] = &job
	}
	return r
}

// SetOnChange installs a callback invoked whenever a job leaves the running
// state (finished with an exit code, or stopped). The scheduler uses it to
// fold real command outcomes back into schedule history. Call it once at
// startup, before any job can run.
func (r *Registry) SetOnChange(fn func(serverID, jobID, status string, exitCode *int)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

func (r *Registry) notifyChange(serverID, id, status string, exitCode *int) {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn(serverID, id, status, exitCode)
	}
}

// ServersWithRunningScheduleJobs returns the distinct server ids that still
// have running jobs started by a schedule — the set a status reconciler needs
// to refresh so finished runs update their schedules' history.
func (r *Registry) ServersWithRunningScheduleJobs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]bool)
	out := []string{}
	for _, j := range r.jobs {
		if j.Status == StatusRunning && j.ScheduleID != "" && !seen[j.ServerID] {
			seen[j.ServerID] = true
			out = append(out, j.ServerID)
		}
	}
	return out
}

// List returns a server's jobs, newest first.
func (r *Registry) List(serverID string) []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0)
	for _, j := range r.jobs {
		if j.ServerID == serverID {
			out = append(out, cloneJob(j))
		}
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].StartedAt == out[k].StartedAt {
			return out[i].ID < out[k].ID
		}
		return out[i].StartedAt > out[k].StartedAt
	})
	return out
}

// Get returns a copy of one job.
func (r *Registry) Get(serverID, id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[jobKey(serverID, id)]
	if !ok {
		return Job{}, false
	}
	return cloneJob(j), true
}

// Start launches a detached tmux job and records it.
func (r *Registry) Start(ctx context.Context, runner Runner, serverID string, req StartRequest) (Job, error) {
	command := strings.TrimSpace(req.Command)
	if command == "" {
		return Job{}, fmt.Errorf("%w: command is required", ErrInvalidJob)
	}
	if len(command) > maxCommandLength {
		return Job{}, fmt.Errorf("%w: command is too long", ErrInvalidJob)
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > maxLabelLength {
		return Job{}, fmt.Errorf("%w: label is too long", ErrInvalidJob)
	}

	id := strings.TrimSpace(req.Session)
	fixed := id != ""
	if fixed {
		if err := config.ValidateID(id); err != nil {
			return Job{}, fmt.Errorf("%w: session: %v", ErrInvalidJob, err)
		}
	}

	r.startMu.Lock()
	defer r.startMu.Unlock()

	if !fixed {
		for attempt := 0; attempt < 5; attempt++ {
			id = r.newID(label)
			r.mu.Lock()
			_, exists := r.jobs[jobKey(serverID, id)]
			r.mu.Unlock()
			if !exists {
				break
			}
			id = ""
		}
		if id == "" {
			return Job{}, fmt.Errorf("could not allocate a unique job id")
		}
	}

	r.mu.Lock()
	if _, exists := r.jobs[jobKey(serverID, id)]; exists {
		r.mu.Unlock()
		return Job{}, fmt.Errorf("%w: %s", ErrSessionExists, sessionName(id))
	}
	r.mu.Unlock()

	if fixed {
		exists, err := r.remoteSessionExists(ctx, runner, id)
		if err != nil {
			return Job{}, err
		}
		if exists {
			return Job{}, fmt.Errorf("%w: %s", ErrSessionExists, sessionName(id))
		}
	}

	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = remoteJobsDir
	}
	job := Job{
		ID:         id,
		ServerID:   serverID,
		Label:      label,
		Command:    command,
		Session:    sessionName(id),
		Dir:        dir,
		LogPath:    logPath(dir, id),
		StatusPath: statusPath(dir, id),
		StartedAt:  r.now().Unix(),
		Status:     StatusRunning,
		Source:     strings.TrimSpace(req.Source),
		ScheduleID: strings.TrimSpace(req.ScheduleID),
	}

	script := launchScript(job, command)
	launchOut, err := runner.RunCommand(ctx, "/bin/sh -c "+shellQuote(script))
	if err != nil {
		return Job{}, fmt.Errorf("launching job: %w (output: %s)", err, launchOut)
	}

	r.mu.Lock()
	r.jobs[jobKey(serverID, id)] = &job
	r.persistLocked()
	out := cloneJob(&job)
	r.mu.Unlock()
	return out, nil
}

// Stop kills the job's tmux session and records it as stopped. Jobs that
// already exited keep their recorded status.
func (r *Registry) Stop(ctx context.Context, runner Runner, serverID, id string) (Job, error) {
	r.mu.Lock()
	j, ok := r.jobs[jobKey(serverID, id)]
	if !ok {
		r.mu.Unlock()
		return Job{}, ErrJobNotFound
	}
	job := cloneJob(j)
	r.mu.Unlock()

	script := stopScript(job)
	if _, err := runner.RunCommand(ctx, "/bin/sh -c "+shellQuote(script)); err != nil {
		return Job{}, fmt.Errorf("stopping job: %w", err)
	}

	r.mu.Lock()
	stopped := false
	if cur, exists := r.jobs[jobKey(serverID, id)]; exists {
		if cur.Status == StatusRunning {
			cur.Status = StatusStopped
			cur.FinishedAt = r.now().Unix()
			stopped = true
		}
		r.persistLocked()
		job = cloneJob(cur)
	}
	r.mu.Unlock()
	if stopped {
		r.notifyChange(serverID, id, job.Status, job.ExitCode)
	}
	return job, nil
}

// Dismiss removes the remote session, log, and status files, then forgets the
// job.
func (r *Registry) Dismiss(ctx context.Context, runner Runner, serverID, id string) error {
	r.mu.Lock()
	j, ok := r.jobs[jobKey(serverID, id)]
	if !ok {
		r.mu.Unlock()
		return ErrJobNotFound
	}
	job := cloneJob(j)
	r.mu.Unlock()

	if _, err := runner.RunCommand(ctx, "/bin/sh -c "+shellQuote(dismissScript(job))); err != nil {
		return fmt.Errorf("dismissing job: %w", err)
	}

	r.mu.Lock()
	delete(r.jobs, jobKey(serverID, id))
	r.stopStreamLocked(serverID, id)
	r.persistLocked()
	r.mu.Unlock()
	return nil
}

// Refresh reconciles running jobs for one server against remote tmux and the
// status files. Call it before listing jobs or opening an observation.
func (r *Registry) Refresh(ctx context.Context, runner Runner, serverID string) error {
	r.mu.Lock()
	var running []Job
	for _, j := range r.jobs {
		if j.ServerID == serverID && j.Status == StatusRunning {
			running = append(running, cloneJob(j))
		}
	}
	r.mu.Unlock()
	if len(running) == 0 {
		return nil
	}

	out, err := runner.RunCommand(ctx, "/bin/sh -c "+shellQuote(refreshScript(running)))
	if err != nil {
		return fmt.Errorf("refreshing jobs: %w", err)
	}

	changed := false
	var finished []Job
	r.mu.Lock()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(parts) < 2 {
			continue
		}
		state, id := parts[0], parts[1]
		j, ok := r.jobs[jobKey(serverID, id)]
		if !ok || j.Status != StatusRunning {
			continue
		}
		switch state {
		case "e":
			j.Status = StatusExited
			j.FinishedAt = r.now().Unix()
			if len(parts) == 3 {
				if exitCode, err := strconv.Atoi(parts[2]); err == nil {
					j.ExitCode = &exitCode
				}
			}
			finished = append(finished, cloneJob(j))
			changed = true
		case "s":
			j.Status = StatusStopped
			j.FinishedAt = r.now().Unix()
			changed = true
			finished = append(finished, cloneJob(j))
		}
	}
	if changed {
		r.persistLocked()
	}
	r.mu.Unlock()
	for _, j := range finished {
		r.notifyChange(serverID, j.ID, j.Status, j.ExitCode)
	}
	return nil
}

// Subscribe opens (or joins) the output stream for one job. The returned
// channel closes when the job's tail command ends, the last subscriber leaves,
// or the registry drops the job.
func (r *Registry) Subscribe(runner Runner, serverID, id string) (<-chan []byte, func(), error) {
	key := jobKey(serverID, id)

	r.mu.Lock()
	j, ok := r.jobs[key]
	if !ok {
		r.mu.Unlock()
		return nil, nil, ErrJobNotFound
	}
	st := r.streams[key]
	created := false
	job := cloneJob(j)
	var ctx context.Context
	if st == nil {
		if r.observerCounts[serverID] >= maxObservers {
			r.mu.Unlock()
			return nil, nil, ErrTooManyObservers
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		st = &stream{subs: make(map[chan []byte]struct{}), cancel: cancel}
		r.streams[key] = st
		r.observerCounts[serverID]++
		created = true
	}
	ch := make(chan []byte, 32)
	st.add(ch)
	r.mu.Unlock()

	if created {
		go r.runStream(ctx, runner, job, key, st)
	}

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			r.mu.Lock()
			remaining := st.remove(ch)
			if remaining == 0 && r.streams[key] == st {
				delete(r.streams, key)
				r.dropObserverLocked(serverID)
				st.cancel()
			}
			r.mu.Unlock()
		})
	}
	return ch, unsubscribe, nil
}

// ForgetServer drops cached jobs for a deleted server without running remote
// cleanup. The remote tmux sessions (if any) are left untouched.
func (r *Registry) ForgetServer(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, j := range r.jobs {
		if j.ServerID == serverID {
			delete(r.jobs, key)
		}
	}
	for key := range r.streams {
		if strings.HasPrefix(key, serverID+"\x00") {
			if st := r.streams[key]; st != nil {
				st.cancel()
				st.closeAll()
			}
			delete(r.streams, key)
		}
	}
	delete(r.observerCounts, serverID)
	r.persistLocked()
}

func (r *Registry) remoteSessionExists(ctx context.Context, runner Runner, id string) (bool, error) {
	script := "if command -v tmux >/dev/null 2>&1; then tmux has-session -t " +
		shellQuote(sessionName(id)) + " 2>/dev/null && echo exists || true; fi"
	out, err := runner.RunCommand(ctx, "/bin/sh -c "+shellQuote(script))
	if err != nil {
		return false, fmt.Errorf("checking tmux session: %w", err)
	}
	return strings.TrimSpace(out) == "exists", nil
}

func (r *Registry) persistLocked() {
	if r.path == "" {
		return
	}
	file := jobsFile{Version: 1, Jobs: make([]Job, 0, len(r.jobs))}
	keys := make([]string, 0, len(r.jobs))
	for key := range r.jobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		file.Jobs = append(file.Jobs, cloneJob(r.jobs[key]))
	}
	if err := storage.WriteJSON(r.path, file); err != nil {
		log.Printf("[jobs] could not save %s: %v", r.path, err)
	}
}

func (r *Registry) dropObserverLocked(serverID string) {
	if n := r.observerCounts[serverID] - 1; n <= 0 {
		delete(r.observerCounts, serverID)
	} else {
		r.observerCounts[serverID] = n
	}
}

func (r *Registry) stopStreamLocked(serverID, id string) {
	key := jobKey(serverID, id)
	if st := r.streams[key]; st != nil {
		delete(r.streams, key)
		r.dropObserverLocked(serverID)
		st.cancel()
		st.closeAll()
	}
}

func (r *Registry) runStream(ctx context.Context, runner Runner, job Job, key string, st *stream) {
	defer func() {
		st.closeAll()
		r.mu.Lock()
		if r.streams[key] == st {
			delete(r.streams, key)
			r.dropObserverLocked(job.ServerID)
		}
		r.mu.Unlock()
	}()

	rc, err := runner.StreamCommand(ctx, "/bin/sh -c "+shellQuote(observeScript(job)))
	if err != nil {
		st.broadcast([]byte("[observe] " + err.Error() + "\r\n"))
		return
	}
	defer rc.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			st.broadcast(chunk)
		}
		if err != nil {
			return
		}
	}
}

// stream fans one remote tail out to all browsers watching a job.
type stream struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	cancel context.CancelFunc
	closed bool
}

func (s *stream) add(ch chan []byte) {
	s.mu.Lock()
	if s.closed {
		close(ch)
	} else {
		s.subs[ch] = struct{}{}
	}
	s.mu.Unlock()
}

func (s *stream) remove(ch chan []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
	return len(s.subs)
}

func (s *stream) broadcast(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for ch := range s.subs {
		select {
		case ch <- data:
		default: // slow browser: drop this chunk rather than stall the tail
		}
	}
}

func (s *stream) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
}

// ── Remote shell construction ────────────────────────────────────────────────

func sessionName(id string) string     { return "serverhub-" + id }
func logPath(dir, id string) string    { return dir + "/" + id + ".log" }
func statusPath(dir, id string) string { return dir + "/" + id + ".status" }
func markerPath(dir, id string) string { return dir + "/" + id + ".go" }

func launchScript(job Job, command string) string {
	marker := markerPath(job.Dir, job.ID)
	parentChmod := ""
	if job.Dir == remoteJobsDir {
		parentChmod = "chmod 700 \"$HOME/.serverhub\"\n"
	}
	inner := fmt.Sprintf(`i=0
while [ ! -f "%s" ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i+1)); done
rm -f "%s"
(
%s
)
printf '%%s\n' "$?" > "%s"`, marker, marker, command, job.StatusPath)

	return fmt.Sprintf(`set -e
command -v tmux >/dev/null 2>&1 || { echo "tmux is required for detached jobs" >&2; exit 127; }
umask 077
mkdir -p "%s"
chmod 700 "%s"
%s: > "%s"
chmod 600 "%s"
rm -f "%s" "%s"
tmux new-session -d -s %s -- sh -lc %s
tmux set-option -t %s remain-on-exit on
tmux pipe-pane -t %s -o 'cat >> "%s"'
: > "%s"`,
		job.Dir, job.Dir, parentChmod, job.LogPath, job.LogPath, job.StatusPath, marker,
		shellQuote(job.Session), shellQuote(inner),
		shellQuote(job.Session), shellQuote(job.Session), job.LogPath, marker)
}

func stopScript(job Job) string {
	return fmt.Sprintf(`if command -v tmux >/dev/null 2>&1; then tmux kill-session -t %s 2>/dev/null || true; fi
if [ ! -f "%s" ]; then printf '143\n' > "%s"; chmod 600 "%s" 2>/dev/null || true; fi`,
		shellQuote(job.Session), job.StatusPath, job.StatusPath, job.StatusPath)
}

func dismissScript(job Job) string {
	return fmt.Sprintf(`if command -v tmux >/dev/null 2>&1; then tmux kill-session -t %s 2>/dev/null || true; fi
rm -f "%s" "%s" "%s"`,
		shellQuote(job.Session), job.LogPath, job.StatusPath, markerPath(job.Dir, job.ID))
}

func refreshScript(running []Job) string {
	var b strings.Builder
	b.WriteString("for id in")
	for _, j := range running {
		b.WriteString(" ")
		b.WriteString(shellQuote(j.ID))
	}
	b.WriteString(`; do
  s="serverhub-$id"
  if command -v tmux >/dev/null 2>&1 && tmux has-session -t "$s" 2>/dev/null; then
    dead=$(tmux list-panes -t "$s" -F '#{pane_dead}' 2>/dev/null | head -n 1)
    if [ "$dead" != "1" ]; then
      echo "r $id"
      continue
    fi
  fi`)
	for _, j := range running {
		fmt.Fprintf(&b, `
  if [ "$id" = %s ] && [ -f "%s" ]; then
    code=$(cat "%s" 2>/dev/null || true)
    echo "e $id $code"
    continue
  fi`, shellQuote(j.ID), j.StatusPath, j.StatusPath)
	}
	b.WriteString(`
  echo "s $id"
done`)
	return b.String()
}

func observeScript(job Job) string {
	return fmt.Sprintf(`exec 2>&1
tail -n +1 -F -- "%s" &
tp=$!
while [ ! -f "%s" ]; do sleep 1; done
sleep 1
kill "$tp" 2>/dev/null || true
wait "$tp" 2>/dev/null || true`,
		job.LogPath, job.StatusPath)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func newJobID(label string) string {
	prefix := config.Slugify(label)
	if prefix == "" {
		prefix = "job"
	}
	if len(prefix) > 40 {
		prefix = strings.TrimRight(prefix[:40], "-")
	}
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("%s-%s", prefix, time.Now().Format("20060102-150405"))
	}
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().Format("20060102"), hex.EncodeToString(suffix))
}

func cloneJob(j *Job) Job {
	c := *j
	if j.ExitCode != nil {
		v := *j.ExitCode
		c.ExitCode = &v
	}
	return c
}
