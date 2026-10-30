// Package scheduler runs schedule definitions from schedules.conf. A single
// ticker goroutine computes due schedules and starts one detached tmux job per
// target through the jobs registry. Runtime state (last runs, history, missed
// occurrences) lives in schedules-state.json, never in the config file.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"serverhub/internal/config"
	"serverhub/internal/jobs"
	"serverhub/internal/storage"
)

const (
	defaultTick     = 10 * time.Second
	defaultGrace    = 30 * time.Second
	defaultJitter   = 1500 * time.Millisecond
	jobStartTimeout = 30 * time.Second
	maxHistory      = 20
	maxSkipped      = 100
	minEvery        = 10 * time.Second
	maxMissedScan   = 1000
	maxOccurrences  = 5000
	// Acute completion watching: after a schedule starts a job, poll its
	// status at watchInterval so finished commands update history within
	// seconds. After watchPolls checks without completion, the slow
	// reconciler keeps the job covered (long-running jobs are not polled
	// aggressively).
	watchInterval = 5 * time.Second
	watchPolls    = 12
)

var (
	ErrNotFound = errors.New("schedule not found")
	ErrInvalid  = errors.New("invalid schedule")
	ErrConflict = errors.New("schedule already exists")
)

// Definition is one schedule from schedules.conf.
type Definition struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Enabled     *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Targets     []string `yaml:"targets,omitempty" json:"targets,omitempty"`
	Tags        []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	QuickAction string   `yaml:"quick_action,omitempty" json:"quick_action,omitempty"`
	Command     string   `yaml:"command,omitempty" json:"command,omitempty"`
	Date        string   `yaml:"date,omitempty" json:"date,omitempty"` // YYYY-MM-DD, one-off run
	Days        []string `yaml:"days,omitempty" json:"days,omitempty"` // mon..sun, weekly cadence
	Every       string   `yaml:"every,omitempty" json:"every,omitempty"`
	At          string   `yaml:"at,omitempty" json:"at,omitempty"`
	Cron        string   `yaml:"cron,omitempty" json:"cron,omitempty"`
	Overlap     string   `yaml:"overlap,omitempty" json:"overlap,omitempty"` // skip (default) | allow
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Target is the scheduler's view of one server.
type Target struct {
	ID           string
	Tags         []string
	QuickActions []config.QuickAction
}

// Env is implemented by the API hub.
type Env interface {
	ScheduleTargets() []Target
	RunScheduleJob(ctx context.Context, serverID string, req jobs.StartRequest) (jobs.Job, error)
	ScheduleJobStatus(serverID, jobID string) (string, bool)
}

// TargetRun is the outcome of one target inside a run.
type TargetRun struct {
	ServerID string `json:"server_id"`
	JobID    string `json:"job_id,omitempty"`
	Status   string `json:"status"` // started | skipped | error
	Error    string `json:"error,omitempty"`
}

// Run is one schedule execution (scheduled or manual).
type Run struct {
	Time    int64       `json:"time"`
	Trigger string      `json:"trigger"` // schedule | manual
	Status  string      `json:"status"`  // success | partial | error | skipped | missed
	Summary string      `json:"summary,omitempty"`
	Targets []TargetRun `json:"targets,omitempty"`
}

// ScheduleState is the persisted runtime state for one schedule.
type ScheduleState struct {
	CreatedAt     int64             `json:"created_at,omitempty"`
	LastScheduled int64             `json:"last_scheduled,omitempty"`
	LastRun       int64             `json:"last_run,omitempty"`
	LastStatus    string            `json:"last_status,omitempty"`
	LastSummary   string            `json:"last_summary,omitempty"`
	LastJobs      map[string]string `json:"last_jobs,omitempty"`
	Skipped       []int64           `json:"skipped,omitempty"`
	EndsAt        int64             `json:"ends_at,omitempty"`
	History       []Run             `json:"history,omitempty"`
}

// View is the API-facing schedule (definition + runtime state).
type View struct {
	Definition
	NextRun     int64             `json:"next_run,omitempty"`
	NextRuns    []int64           `json:"next_runs,omitempty"`
	EndsAt      int64             `json:"ends_at,omitempty"`
	Running     bool              `json:"running,omitempty"`
	LastJobs    map[string]string `json:"-"`
	LastRun     int64             `json:"last_run,omitempty"`
	LastStatus  string            `json:"last_status,omitempty"`
	LastSummary string            `json:"last_summary,omitempty"`
	History     []Run             `json:"history,omitempty"`
}

// Occurrence is one computed run time for the calendar.
type Occurrence struct {
	ScheduleID string `json:"schedule_id"`
	Time       int64  `json:"time"`
	Enabled    bool   `json:"enabled"`
}

// Preview is the builder's dry-run result for an unsaved definition.
type Preview struct {
	NextRuns []int64  `json:"next_runs"`
	Targets  []string `json:"targets"`
	Missing  []string `json:"missing"`
	Warnings []string `json:"warnings,omitempty"`
}

type definitionsFile struct {
	Schedules []Definition `yaml:"schedules"`
}

type stateFile struct {
	Version   int                       `json:"version"`
	Schedules map[string]*ScheduleState `json:"schedules"`
}

type compiled struct {
	date    time.Time // zero unless this is a one-off schedule
	days    map[time.Weekday]bool
	every   time.Duration
	atMin   int // -1 when not set
	timeout time.Duration
	cron    *cronSpec
}

// Scheduler owns the definitions, runtime state, and ticker.
type Scheduler struct {
	defPath   string
	statePath string
	env       Env
	audit     func(serverID, action string)

	interval time.Duration
	grace    time.Duration
	jitter   func() time.Duration
	now      func() time.Time

	watchInterval time.Duration // tests override; defaults to watchInterval

	mu         sync.Mutex
	defs       []Definition
	state      stateFile
	startSlots map[string]chan struct{}
	pending    map[string]bool // "schedule\x00server" starts in flight, for overlap checks
	loadWarn   []string        // non-fatal problems with schedules.conf at load
	stop       chan struct{}
	stopOnce   sync.Once
}

// New loads schedules.conf and schedules-state.json. Neither file being
// corrupt brings the app down: unparseable or invalid entries are skipped and
// reported through ConfigErrors instead.
func New(defPath, statePath string, env Env) (*Scheduler, error) {
	s := &Scheduler{
		defPath:       defPath,
		statePath:     statePath,
		env:           env,
		interval:      defaultTick,
		grace:         defaultGrace,
		jitter:        func() time.Duration { return time.Duration(rand.Int63n(int64(defaultJitter) + 1)) },
		now:           time.Now,
		watchInterval: watchInterval,
		startSlots:    make(map[string]chan struct{}),
		pending:       make(map[string]bool),
		stop:          make(chan struct{}),
		state:         stateFile{Version: 1, Schedules: make(map[string]*ScheduleState)},
	}

	defs, warnings := LoadDefinitions(defPath)
	s.defs = defs
	s.loadWarn = warnings
	if len(warnings) > 0 {
		log.Printf("[scheduler] schedules.conf problems (%d):", len(warnings))
		for _, w := range warnings {
			log.Printf("[scheduler]   %s", w)
		}
	}
	s.loadState()
	s.initializeStateLocked()
	return s, nil
}

// ConfigErrors reports non-fatal problems found while loading schedules.conf.
func (s *Scheduler) ConfigErrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.loadWarn...)
}

// DefinitionsPath returns schedules.conf next to configPath.
func DefinitionsPath(configPath string) string { return config.SchedulesPath(configPath) }

// StatePath returns schedules-state.json in dir.
func StatePath(dir string) string { return filepath.Join(dir, "schedules-state.json") }

// SetAudit wires in the audit logger without importing the API package.
func (s *Scheduler) SetAudit(fn func(serverID, action string)) { s.audit = fn }

func (s *Scheduler) auditAction(serverID, action string) {
	if s.audit != nil {
		s.audit(serverID, action)
	}
}

func (s *Scheduler) loadState() {
	if s.statePath == "" {
		return
	}
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("[scheduler] could not read %s: %v", s.statePath, err)
		}
		return
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("[scheduler] corrupt %s ignored: %v", s.statePath, err)
		return
	}
	if st.Schedules == nil {
		st.Schedules = make(map[string]*ScheduleState)
	}
	st.Version = 1
	s.state = st
}

// initializeStateLocked gives interval-only schedules a stable baseline so a
// restart does not postpone (or immediately trigger) their first run.
func (s *Scheduler) initializeStateLocked() {
	changed := false
	now := s.now()
	for _, def := range s.defs {
		st := s.stateForLocked(def.ID)
		if st.CreatedAt == 0 {
			if st.LastScheduled > 0 {
				st.CreatedAt = st.LastScheduled
			} else {
				st.CreatedAt = now.Unix()
			}
			changed = true
		}
		if st.LastScheduled != 0 {
			continue
		}
		c, err := compile(def)
		if err != nil {
			continue
		}
		if !c.date.IsZero() || len(c.days) > 0 || c.cron != nil || c.atMin >= 0 {
			continue
		}
		st.LastScheduled = now.Unix()
		changed = true
	}
	if changed {
		s.saveStateLocked()
	}
}

// Start begins the ticker goroutine.
func (s *Scheduler) Start() { go s.loop() }

// Stop ends the ticker goroutine. Safe to call more than once.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Scheduler) loop() {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

// List returns every definition with runtime state and the next due time.
func (s *Scheduler) List() []View {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.defs))
	for _, def := range s.defs {
		out = append(out, s.viewLocked(def, now))
	}
	return out
}

// Get returns one schedule view.
func (s *Scheduler) Get(id string) (View, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.definitionLocked(id)
	if !ok {
		return View{}, false
	}
	return s.viewLocked(def, now), true
}

func (s *Scheduler) viewLocked(def Definition, now time.Time) View {
	st := s.stateForLocked(def.ID)
	v := View{
		Definition:  cloneDefinition(def),
		LastRun:     st.LastRun,
		LastStatus:  st.LastStatus,
		LastSummary: st.LastSummary,
		History:     append([]Run(nil), st.History...),
		LastJobs:    cloneStringMap(st.LastJobs),
		EndsAt:      st.EndsAt,
	}
	if c, err := compile(def); err == nil {
		runs := occurrenceTimes(def, c, st, now, now.AddDate(5, 0, 0), 5)
		if len(runs) > 0 {
			v.NextRuns = runs
			v.NextRun = runs[0]
		}
	}
	return v
}

// Create adds a schedule. Definitions and state are written atomically.
func (s *Scheduler) Create(def Definition) (View, error) {
	normalized, err := normalizeOne(def)
	if err != nil {
		return View{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.definitionLocked(normalized.ID); exists {
		return View{}, fmt.Errorf("%w: %s", ErrConflict, normalized.ID)
	}
	s.defs = append(s.defs, normalized)
	st := s.stateForLocked(normalized.ID)
	now := s.now()
	st.CreatedAt = now.Unix()
	if c, err := compile(normalized); err == nil && c.date.IsZero() && len(c.days) == 0 && c.cron == nil && c.atMin < 0 {
		st.LastScheduled = now.Unix()
	}
	if err := s.saveDefinitionsLocked(); err != nil {
		s.removeDefinitionLocked(normalized.ID)
		return View{}, err
	}
	if err := s.saveStateLocked(); err != nil {
		return View{}, err
	}
	return s.viewLocked(normalized, s.now()), nil
}

// Update replaces a definition (ID is immutable) while keeping runtime state.
func (s *Scheduler) Update(id string, def Definition) (View, error) {
	def.ID = id
	normalized, err := normalizeOne(def)
	if err != nil {
		return View{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.definitionIndexLocked(id)
	if idx == -1 {
		return View{}, ErrNotFound
	}
	old := s.defs[idx]
	s.defs[idx] = normalized
	if err := s.saveDefinitionsLocked(); err != nil {
		s.defs[idx] = old
		return View{}, err
	}
	st := s.stateForLocked(id)
	if !sameCadence(old, normalized) {
		st.LastScheduled = 0
		st.Skipped = nil
		st.EndsAt = 0
		if c, err := compile(normalized); err == nil && c.date.IsZero() && len(c.days) == 0 && c.cron == nil && c.atMin < 0 {
			st.LastScheduled = s.now().Unix()
		}
	}
	if err := s.saveStateLocked(); err != nil {
		return View{}, err
	}
	return s.viewLocked(normalized, s.now()), nil
}

// DeleteOccurrence removes one run (future=false) or the run and everything
// after it (future=true). Past occurrences remain visible in the calendar.
func (s *Scheduler) DeleteOccurrence(id string, timestamp int64, future bool) (View, error) {
	if timestamp <= 0 {
		return View{}, fmt.Errorf("%w: occurrence time is required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.definitionIndexLocked(id)
	if idx == -1 {
		return View{}, ErrNotFound
	}
	st := s.stateForLocked(id)
	if future {
		st.EndsAt = timestamp
	} else {
		found := false
		for _, ts := range st.Skipped {
			if ts == timestamp {
				found = true
				break
			}
		}
		if !found {
			st.Skipped = append(st.Skipped, timestamp)
			sort.Slice(st.Skipped, func(i, j int) bool { return st.Skipped[i] < st.Skipped[j] })
			// Only future occurrences consult this list; keep it bounded.
			if len(st.Skipped) > maxSkipped {
				st.Skipped = st.Skipped[len(st.Skipped)-maxSkipped:]
			}
		}
	}
	if err := s.saveStateLocked(); err != nil {
		return View{}, err
	}
	return s.viewLocked(s.defs[idx], s.now()), nil
}

// SetEnabled flips a schedule without rewriting its other fields.
func (s *Scheduler) SetEnabled(id string, enabled bool) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.definitionIndexLocked(id)
	if idx == -1 {
		return View{}, ErrNotFound
	}
	old := s.defs[idx]
	updated := cloneDefinition(old)
	v := enabled
	updated.Enabled = &v
	s.defs[idx] = updated
	if err := s.saveDefinitionsLocked(); err != nil {
		s.defs[idx] = old
		return View{}, err
	}
	if enabled {
		st := s.stateForLocked(id)
		if c, err := compile(updated); err == nil {
			if c.cron != nil || (c.date.IsZero() && len(c.days) == 0 && c.atMin < 0) {
				nowT := s.now()
				skippedN := 0
				if st.LastScheduled > 0 {
					runs := occurrenceTimes(updated, c, st, time.Unix(st.LastScheduled, 0), nowT, maxMissedScan)
					skippedN = len(runs)
				}
				st.LastScheduled = nowT.Unix()
				if skippedN > 0 {
					summary := fmt.Sprintf("%d run(s) skipped while paused", skippedN)
					s.appendHistoryLocked(st, Run{
						Time:    nowT.Unix(),
						Trigger: "pause",
						Status:  "skipped",
						Summary: summary,
					})
				}
			}
		}
	}
	if err := s.saveStateLocked(); err != nil {
		return View{}, err
	}
	return s.viewLocked(updated, s.now()), nil
}

// Delete removes a definition and its runtime state.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.definitionIndexLocked(id)
	if idx == -1 {
		return ErrNotFound
	}
	old := s.defs[idx]
	oldState := s.state.Schedules[id]
	s.defs = append(s.defs[:idx], s.defs[idx+1:]...)
	delete(s.state.Schedules, id)
	if err := s.saveDefinitionsLocked(); err != nil {
		s.defs = append(s.defs[:idx], append([]Definition{old}, s.defs[idx:]...)...)
		if oldState != nil {
			s.state.Schedules[id] = oldState
		}
		return err
	}
	return s.saveStateLocked()
}

// RunNow starts a schedule immediately, bypassing the timer and overlap policy
// (a fixed tmux session can still reject a duplicate start).
func (s *Scheduler) RunNow(ctx context.Context, id string) (Run, error) {
	s.mu.Lock()
	def, ok := s.definitionLocked(id)
	if !ok {
		s.mu.Unlock()
		return Run{}, ErrNotFound
	}
	s.mu.Unlock()
	c, err := compile(def)
	if err != nil {
		return Run{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.execute(ctx, def, c, "manual", true), nil
}

func (s *Scheduler) tick(now time.Time) {
	type due struct {
		def Definition
		c   compiled
	}
	var runnable []due
	changed := false

	s.mu.Lock()
	for _, def := range s.defs {
		if !enabled(def) {
			continue
		}
		c, err := compile(def)
		if err != nil {
			continue
		}
		st := s.stateForLocked(def.ID)
		missed := 0
		for {
			next := s.nextDue(c, st, now)
			if next.IsZero() {
				break
			}
			if next.After(now.Add(-s.grace)) {
				if !next.After(now) {
					st.LastScheduled = next.Unix()
					runnable = append(runnable, due{def: def, c: c})
					changed = true
				}
				break
			}
			// More than the grace window late: skip the occurrence.
			st.LastScheduled = next.Unix()
			missed++
			changed = true
			if missed >= maxMissedScan {
				st.LastScheduled = now.Unix()
				break
			}
		}
		if missed > 0 {
			st.LastRun = now.Unix()
			st.LastStatus = "missed"
			st.LastSummary = fmt.Sprintf("%d missed run(s)", missed)
			s.appendHistoryLocked(st, Run{
				Time:    now.Unix(),
				Trigger: "schedule",
				Status:  "missed",
				Summary: st.LastSummary,
			})
		}
	}
	if changed {
		s.saveStateLocked()
	}
	s.mu.Unlock()

	for _, r := range runnable {
		go s.execute(context.Background(), r.def, r.c, "schedule", false)
	}
}

// execute resolves targets and starts one job per target.
func (s *Scheduler) execute(ctx context.Context, def Definition, c compiled, trigger string, manual bool) Run {
	started := s.now()
	run := Run{Time: started.Unix(), Trigger: trigger}
	run.Targets = append(run.Targets, s.runTargets(ctx, def, c, manual)...)

	run.Status, run.Summary = aggregate(run.Targets)
	s.mu.Lock()
	st := s.stateForLocked(def.ID)
	st.LastRun = run.Time
	st.LastStatus = run.Status
	st.LastSummary = run.Summary
	s.appendHistoryLocked(st, run)
	s.saveStateLocked()
	s.mu.Unlock()
	for _, tr := range run.Targets {
		if tr.Status == "started" && tr.JobID != "" {
			go s.watchJob(def.ID, tr.ServerID, tr.JobID)
		}
	}
	return run
}

// watchJob polls a schedule job's status right after it starts, so a command
// that finishes within seconds updates the schedule's history promptly (the
// jobs registry's change hook folds the exit code in as a side effect of the
// refresh). If the job is still running after the acute window, the periodic
// reconciler keeps it covered and this watcher bows out.
func (s *Scheduler) watchJob(defID, serverID, jobID string) {
	interval := s.watchInterval
	for range watchPolls {
		select {
		case <-s.stop:
			return
		case <-time.After(interval):
		}
		// The env's status check refreshes the registry (SSH round trip); it
		// manages its own timeouts.
		status, ok := s.env.ScheduleJobStatus(serverID, jobID)
		if !ok {
			// Job vanished from the registry (dismissed, server forgotten):
			// nothing left to watch or fold in.
			s.auditAction(serverID, "schedule "+defID+" watch: job "+jobID+" no longer tracked")
			return
		}
		if status != jobs.StatusRunning {
			return
		}
	}
}

func (s *Scheduler) runTargets(ctx context.Context, def Definition, c compiled, manual bool) []TargetRun {
	targets, missing := s.resolveTargets(def)
	results := make([]TargetRun, 0, len(targets)+len(missing))
	for _, m := range missing {
		results = append(results, TargetRun{ServerID: m, Status: "error", Error: "target not found"})
		s.auditAction("scheduler", "schedule target missing: "+def.ID+" -> "+m)
	}
	if len(targets) == 0 && len(missing) == 0 {
		results = append(results, TargetRun{Status: "error", Error: "no targets resolved"})
	}

	for _, target := range targets {
		if !manual {
			if d := s.jitter(); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return results
				}
			}
		}
		if err := s.acquireSlot(ctx, target.ID); err != nil {
			results = append(results, TargetRun{ServerID: target.ID, Status: "error", Error: err.Error()})
			continue
		}
		tr := s.startTarget(ctx, def, c, target, manual)
		s.releaseSlot(target.ID)
		results = append(results, tr)
	}
	return results
}

func (s *Scheduler) startTarget(ctx context.Context, def Definition, c compiled, target Target, manual bool) TargetRun {
	command, session, err := commandFor(def, target)
	if err != nil {
		s.auditAction(target.ID, "schedule error: "+def.ID+": "+err.Error())
		return TargetRun{ServerID: target.ID, Status: "error", Error: err.Error()}
	}

	if !manual && def.Overlap != "allow" {
		if last := s.lastJob(def.ID, target.ID); last != "" {
			if status, ok := s.env.ScheduleJobStatus(target.ID, last); ok && status == jobs.StatusRunning {
				s.auditAction(target.ID, "schedule skipped (already running): "+def.ID)
				return TargetRun{ServerID: target.ID, Status: "skipped", Error: "previous job still running"}
			}
		}
		// Claim atomically before starting: two ticks close together can both
		// pass the still-running check above, but only one can hold the claim.
		if !s.claimStart(def.ID, target.ID) {
			s.auditAction(target.ID, "schedule skipped (start in flight): "+def.ID)
			return TargetRun{ServerID: target.ID, Status: "skipped", Error: "start already in flight"}
		}
		defer s.releaseStart(def.ID, target.ID)
	}

	startCtx, cancel := context.WithTimeout(ctx, jobStartTimeout)
	defer cancel()
	job, err := s.env.RunScheduleJob(startCtx, target.ID, jobs.StartRequest{
		Command:    withTimeout(command, c.timeout),
		Label:      def.Name,
		Session:    session,
		Source:     "schedule",
		ScheduleID: def.ID,
	})
	if err != nil {
		if errors.Is(err, jobs.ErrSessionExists) {
			s.auditAction(target.ID, "schedule skipped (session exists): "+def.ID)
			return TargetRun{ServerID: target.ID, Status: "skipped", Error: "session already exists"}
		}
		s.auditAction(target.ID, "schedule error: "+def.ID+": "+err.Error())
		return TargetRun{ServerID: target.ID, Status: "error", Error: err.Error()}
	}

	s.mu.Lock()
	st := s.stateForLocked(def.ID)
	if st.LastJobs == nil {
		st.LastJobs = make(map[string]string)
	}
	st.LastJobs[target.ID] = job.ID
	s.mu.Unlock()

	s.auditAction(target.ID, "schedule started: "+def.ID)
	return TargetRun{ServerID: target.ID, JobID: job.ID, Status: "started"}
}

func (s *Scheduler) lastJob(scheduleID, serverID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.state.Schedules[scheduleID]; st != nil {
		return st.LastJobs[serverID]
	}
	return ""
}

// claimStart marks a (schedule, server) start as in flight. Returning false
// means another start for the same pair is underway. releaseStart must be
// called when the start attempt finishes, success or failure.
func (s *Scheduler) claimStart(scheduleID, serverID string) bool {
	key := scheduleID + "\x00" + serverID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[key] {
		return false
	}
	s.pending[key] = true
	return true
}

func (s *Scheduler) releaseStart(scheduleID, serverID string) {
	key := scheduleID + "\x00" + serverID
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, key)
}

// JobFinished folds a finished job's real outcome back into the schedule run
// that started it. It is registered as the jobs registry's change hook, so
// history reflects what the command actually did, not merely that it started.
func (s *Scheduler) JobFinished(serverID, jobID, status string, exitCode *int) {
	finalStatus := "failed"
	if status == jobs.StatusExited && exitCode != nil && *exitCode == 0 {
		finalStatus = "done"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range s.state.Schedules {
		if st.LastJobs[serverID] != jobID {
			continue
		}
		for i := range st.History {
			run := &st.History[i]
			updated := false
			for j := range run.Targets {
				tr := &run.Targets[j]
				if tr.ServerID != serverID || jobID == "" || tr.JobID != jobID || tr.Status != "started" {
					continue
				}
				tr.Status = finalStatus
				switch {
				case finalStatus == "done":
					tr.Error = ""
				case status == jobs.StatusStopped:
					tr.Error = "job stopped"
				case exitCode != nil:
					tr.Error = fmt.Sprintf("exit code %d", *exitCode)
				default:
					tr.Error = "job exited without a code"
				}
				updated = true
			}
			if updated {
				run.Status, run.Summary = aggregate(run.Targets)
				if i == 0 {
					// Newest entry: refresh the schedule's headline status.
					st.LastStatus = run.Status
					st.LastSummary = run.Summary
					st.LastRun = run.Time
				}
				s.saveStateLocked()
				s.auditAction(serverID, "schedule "+id+" run "+finalStatus+": exit "+fmtStatus(status, exitCode))
				return
			}
		}
	}
}

func fmtStatus(status string, exitCode *int) string {
	if status == jobs.StatusExited && exitCode != nil {
		return fmt.Sprintf("%d", *exitCode)
	}
	return status
}

// resolveTargets merges explicit IDs and tags at run time. Missing explicit IDs
// (and tags with no matches) are returned so the run can report them.
func (s *Scheduler) resolveTargets(def Definition) ([]Target, []string) {
	all := s.env.ScheduleTargets()
	byID := make(map[string]Target, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}

	seen := make(map[string]bool)
	var targets []Target
	var missing []string
	add := func(t Target) {
		if seen[t.ID] {
			return
		}
		seen[t.ID] = true
		targets = append(targets, t)
	}

	for _, id := range def.Targets {
		if t, ok := byID[id]; ok {
			add(t)
		} else {
			missing = append(missing, id)
		}
	}
	for _, tag := range def.Tags {
		matched := false
		for _, t := range all {
			if contains(t.Tags, tag) {
				add(t)
				matched = true
			}
		}
		if !matched {
			missing = append(missing, "tag:"+tag)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, missing
}

func commandFor(def Definition, t Target) (command, session string, err error) {
	if def.Command != "" {
		return def.Command, "", nil
	}
	for _, qa := range t.QuickActions {
		if qa.ID == def.QuickAction {
			return qa.Command, qa.Session, nil
		}
	}
	return "", "", fmt.Errorf("quick action %q not found on %s", def.QuickAction, t.ID)
}

func (s *Scheduler) acquireSlot(ctx context.Context, serverID string) error {
	s.mu.Lock()
	slot := s.startSlots[serverID]
	if slot == nil {
		slot = make(chan struct{}, 2)
		s.startSlots[serverID] = slot
	}
	s.mu.Unlock()
	select {
	case slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) releaseSlot(serverID string) {
	s.mu.Lock()
	slot := s.startSlots[serverID]
	s.mu.Unlock()
	if slot != nil {
		<-slot
	}
}

// nextDue returns the next occurrence after LastScheduled (or the next
// occurrence after now for a new at/date/day schedule). It may be in the past
// when a run was missed; tick() advances past those.
func (s *Scheduler) nextDue(c compiled, st *ScheduleState, now time.Time) time.Time {
	if st == nil {
		return s.nextDueRaw(c, st, now)
	}
	for i := 0; i < maxMissedScan; i++ {
		next := s.nextDueRaw(c, st, now)
		if next.IsZero() {
			return next
		}
		if st.EndsAt > 0 && next.Unix() >= st.EndsAt {
			return time.Time{}
		}
		if !isSkipped(st, next.Unix()) {
			return next
		}
		st.LastScheduled = next.Unix()
	}
	return time.Time{}
}

// intervalDays returns N when every is an exact multiple of 24h (N >= 1),
// otherwise 0. Whole-day intervals are stepped in wall-clock days so that
// "every 24h" keeps firing at the same local time across DST shifts instead
// of drifting an hour per transition.
func intervalDays(every time.Duration) int {
	if every < 24*time.Hour {
		return 0
	}
	days := int(every / (24 * time.Hour))
	if time.Duration(days)*24*time.Hour == every {
		return days
	}
	return 0
}

// addInterval advances t by one interval, preferring wall-clock day steps
// for exact day multiples.
func addInterval(t time.Time, every time.Duration) time.Time {
	if days := intervalDays(every); days > 0 {
		return t.AddDate(0, 0, days)
	}
	return t.Add(every)
}

func (s *Scheduler) nextDueRaw(c compiled, st *ScheduleState, now time.Time) time.Time {
	loc := now.Location()
	if !c.date.IsZero() {
		if st.LastScheduled >= c.date.Unix() {
			return time.Time{}
		}
		return c.date.In(loc)
	}
	if c.cron != nil {
		if st.LastScheduled > 0 {
			return c.cron.next(time.Unix(st.LastScheduled, 0))
		}
		return c.cron.next(now.Add(-s.grace))
	}
	if len(c.days) > 0 {
		base := now.Add(-s.grace)
		if st.LastScheduled > 0 {
			base = time.Unix(st.LastScheduled, 0).In(loc)
		}
		d := time.Date(base.Year(), base.Month(), base.Day(), c.atMin/60, c.atMin%60, 0, 0, loc)
		if !d.After(base) {
			d = d.AddDate(0, 0, 1)
		}
		for i := 0; i < 370; i++ {
			if c.days[d.Weekday()] {
				return d
			}
			d = d.AddDate(0, 0, 1)
		}
		return time.Time{}
	}
	if c.atMin >= 0 {
		base := now
		if st.LastScheduled > 0 {
			base = time.Unix(st.LastScheduled, 0).In(loc)
		}
		cand := time.Date(base.Year(), base.Month(), base.Day(), c.atMin/60, c.atMin%60, 0, 0, loc)
		if st.LastScheduled > 0 {
			minNext := time.Unix(st.LastScheduled, 0).In(loc).Add(c.every)
			for cand.Before(minNext) {
				cand = cand.AddDate(0, 0, 1)
			}
			return cand
		}
		if cand.After(now) || now.Sub(cand) <= s.grace {
			return cand
		}
		return cand.AddDate(0, 0, 1)
	}
	if st.LastScheduled > 0 {
		return addInterval(time.Unix(st.LastScheduled, 0).In(loc), c.every)
	}
	return now.Add(c.every)
}

// Occurrences returns computed run times in [from, to) for every schedule,
// including disabled ones (so the calendar can dim them).
func (s *Scheduler) Occurrences(from, to time.Time) []Occurrence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Occurrence, 0)
	for _, def := range s.defs {
		c, err := compile(def)
		if err != nil {
			continue
		}
		st := s.stateForLocked(def.ID)
		for _, ts := range occurrenceTimes(def, c, st, from, to, maxOccurrences) {
			out = append(out, Occurrence{
				ScheduleID: def.ID,
				Time:       ts,
				Enabled:    enabled(def),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time == out[j].Time {
			return out[i].ScheduleID < out[j].ScheduleID
		}
		return out[i].Time < out[j].Time
	})
	return out
}

// Preview validates a draft cadence and returns the next five runs plus target
// and quick-action compatibility warnings.
func (s *Scheduler) Preview(def Definition) (Preview, error) {
	def.ID = "preview"
	def.Name = "preview"
	if err := normalizeCadence(&def); err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c, err := compile(def)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	now := s.now()
	preview := Preview{
		NextRuns: occurrenceTimes(def, c, &ScheduleState{CreatedAt: now.Unix()}, now, now.AddDate(5, 0, 0), 5),
		Targets:  []string{},
		Missing:  []string{},
	}
	if len(def.Targets) == 0 && len(def.Tags) == 0 {
		return preview, nil
	}
	targets, missing := s.resolveTargets(def)
	for _, t := range targets {
		preview.Targets = append(preview.Targets, t.ID)
		if def.QuickAction != "" && !targetHasAction(t, def.QuickAction) {
			preview.Warnings = append(preview.Warnings,
				fmt.Sprintf("quick action %q is not available on %s", def.QuickAction, t.ID))
		}
	}
	preview.Missing = append(preview.Missing, missing...)
	return preview, nil
}

// occurrenceTimes expands one definition into unix timestamps in [from, to).
// It is the single source of truth for next-run, calendar, and preview output.
func occurrenceTimes(def Definition, c compiled, st *ScheduleState, from, to time.Time, limit int) []int64 {
	if limit <= 0 {
		limit = maxOccurrences
	}
	if !to.After(from) {
		return nil
	}
	loc := from.Location()
	if st != nil && st.EndsAt > 0 {
		end := time.Unix(st.EndsAt, 0).In(loc)
		if end.Before(to) {
			to = end
		}
		if !to.After(from) {
			return nil
		}
	}
	out := make([]int64, 0, 8)
	add := func(t time.Time) bool {
		if t.Before(from) || !t.Before(to) {
			return true
		}
		if isSkipped(st, t.Unix()) {
			return true
		}
		out = append(out, t.Unix())
		return len(out) < limit
	}

	if !c.date.IsZero() {
		if st == nil || st.LastScheduled < c.date.Unix() {
			add(c.date.In(loc))
		}
		return out
	}

	start := from
	if st != nil && st.CreatedAt > 0 {
		created := time.Unix(st.CreatedAt, 0).In(loc)
		if created.After(start) {
			start = created
		}
	}
	if !start.Before(to) {
		return out
	}

	if c.cron != nil {
		t := c.cron.next(start.Add(-time.Minute))
		for !t.IsZero() && t.Before(to) {
			if !add(t) {
				break
			}
			t = c.cron.next(t)
		}
		return out
	}

	if len(c.days) > 0 {
		d := time.Date(start.Year(), start.Month(), start.Day(), c.atMin/60, c.atMin%60, 0, 0, loc)
		if d.Before(start) {
			d = d.AddDate(0, 0, 1)
		}
		for d.Before(to) {
			if c.days[d.Weekday()] {
				if !add(d) {
					break
				}
			}
			d = d.AddDate(0, 0, 1)
		}
		return out
	}

	if c.atMin >= 0 {
		stepDays := int(c.every / (24 * time.Hour))
		if stepDays < 1 {
			stepDays = 1
		}
		anchor := time.Date(start.Year(), start.Month(), start.Day(), c.atMin/60, c.atMin%60, 0, 0, loc)
		if st != nil && st.CreatedAt > 0 {
			created := time.Unix(st.CreatedAt, 0).In(loc)
			anchor = time.Date(created.Year(), created.Month(), created.Day(), c.atMin/60, c.atMin%60, 0, 0, loc)
		}
		for anchor.Before(start) {
			anchor = anchor.AddDate(0, 0, stepDays)
		}
		for anchor.Before(to) {
			if !add(anchor) {
				break
			}
			anchor = anchor.AddDate(0, 0, stepDays)
		}
		return out
	}

	anchor := start
	if st != nil && st.CreatedAt > 0 {
		anchor = time.Unix(st.CreatedAt, 0).In(loc)
	}
	t := addInterval(anchor, c.every)
	if t.Before(start) {
		if days := intervalDays(c.every); days > 0 {
			n := int(start.Sub(t) / (time.Duration(days) * 24 * time.Hour))
			t = t.AddDate(0, 0, n*days)
		} else {
			n := int(start.Sub(t) / c.every)
			t = t.Add(time.Duration(n) * c.every)
		}
		for t.Before(start) {
			t = addInterval(t, c.every)
		}
	}
	for t.Before(to) {
		if !add(t) {
			break
		}
		t = addInterval(t, c.every)
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isSkipped(st *ScheduleState, ts int64) bool {
	if st == nil {
		return false
	}
	for _, skipped := range st.Skipped {
		if skipped == ts {
			return true
		}
	}
	return false
}

func targetHasAction(t Target, id string) bool {
	for _, qa := range t.QuickActions {
		if qa.ID == id {
			return true
		}
	}
	return false
}

func sameCadence(a, b Definition) bool {
	if a.Date != b.Date || a.Every != b.Every || a.At != b.At || a.Cron != b.Cron {
		return false
	}
	if len(a.Days) != len(b.Days) {
		return false
	}
	for i := range a.Days {
		if a.Days[i] != b.Days[i] {
			return false
		}
	}
	return true
}

func withTimeout(command string, d time.Duration) string {
	if d <= 0 {
		return command
	}
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("timeout %d sh -c %s", secs, shellQuote(command))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func aggregate(results []TargetRun) (status, summary string) {
	var started, skipped, failed int
	for _, r := range results {
		switch r.Status {
		case "started", "done":
			started++
		case "skipped":
			skipped++
		case "error", "failed":
			failed++
		}
	}
	switch {
	case len(results) == 0:
		status = "error"
	case failed == 0 && skipped == 0 && started > 0:
		status = "success"
	case started == 0 && failed == 0 && skipped > 0:
		status = "skipped"
	case started == 0:
		status = "error"
	default:
		status = "partial"
	}
	parts := make([]string, 0, 3)
	if started > 0 {
		parts = append(parts, fmt.Sprintf("%d ok", started))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	summary = strings.Join(parts, ", ")
	if summary == "" {
		summary = "no targets"
	}
	return status, summary
}

func (s *Scheduler) appendHistoryLocked(st *ScheduleState, run Run) {
	st.History = append([]Run{run}, st.History...)
	if len(st.History) > maxHistory {
		st.History = st.History[:maxHistory]
	}
}

func (s *Scheduler) definitionIndexLocked(id string) int {
	for i := range s.defs {
		if s.defs[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Scheduler) definitionLocked(id string) (Definition, bool) {
	idx := s.definitionIndexLocked(id)
	if idx == -1 {
		return Definition{}, false
	}
	return cloneDefinition(s.defs[idx]), true
}

func (s *Scheduler) removeDefinitionLocked(id string) {
	idx := s.definitionIndexLocked(id)
	if idx != -1 {
		s.defs = append(s.defs[:idx], s.defs[idx+1:]...)
	}
}

func (s *Scheduler) stateForLocked(id string) *ScheduleState {
	if s.state.Schedules == nil {
		s.state.Schedules = make(map[string]*ScheduleState)
	}
	st := s.state.Schedules[id]
	if st == nil {
		st = &ScheduleState{}
		s.state.Schedules[id] = st
	}
	return st
}

func (s *Scheduler) saveDefinitionsLocked() error {
	if s.defPath == "" {
		return nil
	}
	if err := SaveDefinitions(s.defPath, s.defs); err != nil {
		log.Printf("[scheduler] could not save %s: %v", s.defPath, err)
		return err
	}
	return nil
}

func (s *Scheduler) saveStateLocked() error {
	if s.statePath == "" {
		return nil
	}
	if err := storage.WriteJSON(s.statePath, s.state); err != nil {
		log.Printf("[scheduler] could not save %s: %v", s.statePath, err)
		return err
	}
	return nil
}

func cloneDefinition(d Definition) Definition {
	c := d
	c.Targets = append([]string(nil), d.Targets...)
	c.Tags = append([]string(nil), d.Tags...)
	c.Days = append([]string(nil), d.Days...)
	if d.Enabled != nil {
		v := *d.Enabled
		c.Enabled = &v
	}
	return c
}

func enabled(d Definition) bool { return d.Enabled == nil || *d.Enabled }

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ── Definitions file + validation ────────────────────────────────────────────

// LoadDefinitions reads and normalises schedules.conf. Invalid entries are
// skipped, not fatal: the returned warnings describe every skipped entry (or
// a wholly unparseable file) so the caller can log and surface them.
func LoadDefinitions(path string) ([]Definition, []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("could not read %s: %v", path, err)}
	}
	if changed, err := storage.FixPermissions(path); err == nil && changed {
		log.Printf("warning: %s was group/world-readable — correcting to 0600", path)
	}
	var f definitionsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, []string{fmt.Sprintf("schedules.conf could not be parsed (ignored): %v", err)}
	}
	return NormalizeDefinitions(f.Schedules)
}

// NormalizeDefinitions trims and validates definitions, de-duplicates IDs and
// targets, and fills defaults. Invalid or duplicate entries are skipped with a
// warning instead of failing the whole file.
func NormalizeDefinitions(list []Definition) ([]Definition, []string) {
	out := make([]Definition, 0, len(list))
	seen := make(map[string]bool, len(list))
	var warnings []string
	for _, def := range list {
		normalized, err := normalizeOne(def)
		if err != nil {
			id := normalized.ID
			if id == "" {
				id = strings.TrimSpace(def.ID)
			}
			if id == "" {
				id = def.Name
			}
			if id == "" {
				id = fmt.Sprintf("entry #%d", len(warnings)+len(out)+1)
			}
			warnings = append(warnings, fmt.Sprintf("skipped schedule %q: %v", id, err))
			continue
		}
		if seen[normalized.ID] {
			warnings = append(warnings, fmt.Sprintf("skipped duplicate schedule id %q", normalized.ID))
			continue
		}
		seen[normalized.ID] = true
		out = append(out, normalized)
	}
	return out, warnings
}

// SaveDefinitions writes schedules.conf atomically (0600).
func SaveDefinitions(path string, defs []Definition) error {
	data, err := yaml.Marshal(definitionsFile{Schedules: defs})
	if err != nil {
		return fmt.Errorf("marshaling schedules: %w", err)
	}
	header := []byte("# ServerHub schedules — managed from the Schedules page\n# Runtime history lives in schedules-state.json.\n\n")
	return storage.Write(path, append(header, data...))
}

func normalizeOne(def Definition) (Definition, error) {
	def.ID = strings.TrimSpace(def.ID)
	def.Name = strings.TrimSpace(def.Name)
	def.QuickAction = strings.TrimSpace(def.QuickAction)
	def.Command = strings.TrimSpace(def.Command)
	def.Date = strings.TrimSpace(def.Date)
	def.Every = strings.TrimSpace(def.Every)
	def.At = strings.TrimSpace(def.At)
	def.Cron = strings.TrimSpace(def.Cron)
	def.Overlap = strings.ToLower(strings.TrimSpace(def.Overlap))
	def.Timeout = strings.TrimSpace(def.Timeout)

	if def.ID == "" {
		def.ID = config.Slugify(def.Name)
	}
	if err := config.ValidateID(def.ID); err != nil {
		return Definition{}, fmt.Errorf("%w: id: %v", ErrInvalid, err)
	}
	if def.Name == "" {
		def.Name = def.ID
	}
	if def.Enabled == nil {
		t := true
		def.Enabled = &t
	}

	def.Targets = cleanList(def.Targets)
	for _, id := range def.Targets {
		if id == "__local__" {
			continue
		}
		if err := config.ValidateID(id); err != nil {
			return Definition{}, fmt.Errorf("%w: target %q: %v", ErrInvalid, id, err)
		}
	}
	def.Tags = cleanList(def.Tags)
	if len(def.Targets) == 0 && len(def.Tags) == 0 {
		return Definition{}, fmt.Errorf("%w: at least one target or tag is required", ErrInvalid)
	}
	if (def.QuickAction == "") == (def.Command == "") {
		return Definition{}, fmt.Errorf("%w: set exactly one of quick_action or command", ErrInvalid)
	}
	if def.QuickAction != "" {
		if err := config.ValidateID(def.QuickAction); err != nil {
			return Definition{}, fmt.Errorf("%w: quick_action: %v", ErrInvalid, err)
		}
	}

	if err := normalizeCadence(&def); err != nil {
		return Definition{}, err
	}

	if def.Overlap == "" {
		def.Overlap = "skip"
	}
	if def.Overlap != "skip" && def.Overlap != "allow" {
		return Definition{}, fmt.Errorf("%w: overlap must be skip or allow", ErrInvalid)
	}
	if def.Timeout != "" {
		d, err := time.ParseDuration(def.Timeout)
		if err != nil || d <= 0 {
			return Definition{}, fmt.Errorf("%w: timeout must be a positive duration", ErrInvalid)
		}
		def.Timeout = d.String()
	}
	return def, nil
}

// normalizeCadence trims and validates the mutually-exclusive cadence fields.
// It is also used by Preview, which has no targets/name/action yet.
func normalizeCadence(def *Definition) error {
	def.Date = strings.TrimSpace(def.Date)
	def.Every = strings.TrimSpace(def.Every)
	def.At = strings.TrimSpace(def.At)
	def.Cron = strings.TrimSpace(def.Cron)
	for _, raw := range def.Days {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if !validDay(d) {
			return fmt.Errorf("%w: invalid day %q", ErrInvalid, raw)
		}
	}
	def.Days = cleanDays(def.Days)

	switch {
	case def.Date != "":
		if _, err := time.Parse("2006-01-02", def.Date); err != nil {
			return fmt.Errorf("%w: date must be YYYY-MM-DD", ErrInvalid)
		}
		if def.At == "" {
			return fmt.Errorf("%w: a one-off schedule needs at (HH:MM)", ErrInvalid)
		}
		if _, err := time.Parse("15:04", def.At); err != nil {
			return fmt.Errorf("%w: at must be HH:MM", ErrInvalid)
		}
		def.Every = ""
		def.Cron = ""
		def.Days = nil

	case len(def.Days) > 0:
		if def.At == "" {
			return fmt.Errorf("%w: a weekly schedule needs at (HH:MM)", ErrInvalid)
		}
		if _, err := time.Parse("15:04", def.At); err != nil {
			return fmt.Errorf("%w: at must be HH:MM", ErrInvalid)
		}
		def.Every = ""
		def.Cron = ""

	case def.Cron != "":
		if _, err := parseCron(def.Cron); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		def.Every = ""
		def.At = ""
		def.Days = nil

	default:
		if def.Every == "" && def.At == "" {
			return fmt.Errorf("%w: date, days, every, at, or cron is required", ErrInvalid)
		}
		if def.Every != "" {
			d, err := time.ParseDuration(def.Every)
			if err != nil || d < minEvery {
				return fmt.Errorf("%w: every must be a duration of at least %s (e.g. 24h)", ErrInvalid, minEvery)
			}
			def.Every = d.String()
		}
		if def.At != "" {
			if _, err := time.Parse("15:04", def.At); err != nil {
				return fmt.Errorf("%w: at must be HH:MM", ErrInvalid)
			}
			if def.Every == "" {
				def.Every = "24h0m0s"
			}
		}
	}
	return nil
}

func validDay(d string) bool {
	switch d {
	case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	}
	return false
}

func cleanDays(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		d := strings.ToLower(strings.TrimSpace(raw))
		switch d {
		case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		default:
			continue
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	order := map[string]int{"mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6, "sun": 7}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

func compile(def Definition) (compiled, error) {
	c := compiled{atMin: -1}
	if def.Cron != "" {
		cron, err := parseCron(def.Cron)
		if err != nil {
			return compiled{}, err
		}
		c.cron = cron
	}
	if def.Date != "" {
		day, err := time.Parse("2006-01-02", def.Date)
		if err != nil {
			return compiled{}, err
		}
		at, err := time.Parse("15:04", def.At)
		if err != nil {
			return compiled{}, err
		}
		c.date = time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, time.Local)
	}
	if len(def.Days) > 0 {
		c.days = make(map[time.Weekday]bool, len(def.Days))
		for _, d := range def.Days {
			switch d {
			case "sun":
				c.days[time.Sunday] = true
			case "mon":
				c.days[time.Monday] = true
			case "tue":
				c.days[time.Tuesday] = true
			case "wed":
				c.days[time.Wednesday] = true
			case "thu":
				c.days[time.Thursday] = true
			case "fri":
				c.days[time.Friday] = true
			case "sat":
				c.days[time.Saturday] = true
			}
		}
	}
	if def.Every != "" {
		d, err := time.ParseDuration(def.Every)
		if err != nil {
			return compiled{}, err
		}
		c.every = d
	}
	if def.At != "" {
		t, err := time.Parse("15:04", def.At)
		if err != nil {
			return compiled{}, err
		}
		c.atMin = t.Hour()*60 + t.Minute()
		if c.every <= 0 && c.date.IsZero() && len(c.days) == 0 {
			c.every = 24 * time.Hour
		}
	}
	if def.Timeout != "" {
		d, err := time.ParseDuration(def.Timeout)
		if err != nil {
			return compiled{}, err
		}
		c.timeout = d
	}
	if c.cron == nil && c.every <= 0 && c.date.IsZero() && len(c.days) == 0 {
		return compiled{}, fmt.Errorf("schedule has no interval")
	}
	if len(c.days) > 0 && c.atMin < 0 {
		return compiled{}, fmt.Errorf("weekly schedule has no time")
	}
	return c, nil
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func cloneState(st *ScheduleState) *ScheduleState {
	if st == nil {
		return nil
	}
	c := *st
	c.History = append([]Run(nil), st.History...)
	if st.LastJobs != nil {
		c.LastJobs = make(map[string]string, len(st.LastJobs))
		for k, v := range st.LastJobs {
			c.LastJobs[k] = v
		}
	}
	return &c
}
