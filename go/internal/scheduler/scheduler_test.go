package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"serverhub/internal/config"
	"serverhub/internal/jobs"
)

type fakeEnv struct {
	mu       sync.Mutex
	targets  []Target
	started  []jobs.StartRequest
	statuses map[string]string
	statusFn func(serverID, jobID string) (string, bool)
	runErr   error
}

func (f *fakeEnv) ScheduleTargets() []Target { return f.targets }

func (f *fakeEnv) RunScheduleJob(_ context.Context, serverID string, req jobs.StartRequest) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return jobs.Job{}, f.runErr
	}
	f.started = append(f.started, req)
	return jobs.Job{
		ID:         fmt.Sprintf("job-%d", len(f.started)),
		ServerID:   serverID,
		Session:    "serverhub-job",
		Command:    req.Command,
		Status:     jobs.StatusRunning,
		ScheduleID: req.ScheduleID,
	}, nil
}

func (f *fakeEnv) ScheduleJobStatus(serverID, jobID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusFn != nil {
		return f.statusFn(serverID, jobID)
	}
	status, ok := f.statuses[serverID+"/"+jobID]
	return status, ok
}

func (f *fakeEnv) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func boolPtr(b bool) *bool { return &b }

func baseDef(id string) Definition {
	return Definition{
		ID:          id,
		Name:        "Nightly",
		Enabled:     boolPtr(true),
		Targets:     []string{"web-1"},
		QuickAction: "restart",
		Every:       "1h",
	}
}

func testTarget() Target {
	return Target{
		ID:   "web-1",
		Tags: []string{"prod"},
		QuickActions: []config.QuickAction{
			{ID: "restart", Label: "Restart", Command: "systemctl restart app", Mode: "detached"},
		},
	}
}

func newTestScheduler(t *testing.T, defs []Definition, env Env) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	defPath := filepath.Join(dir, "schedules.conf")
	if err := SaveDefinitions(defPath, defs); err != nil {
		t.Fatalf("SaveDefinitions: %v", err)
	}
	s, err := New(defPath, filepath.Join(dir, "schedules-state.json"), env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.jitter = func() time.Duration { return 0 }
	return s, dir
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNormalizeDefinition(t *testing.T) {
	ok, err := normalizeOne(baseDef("nightly"))
	if err != nil {
		t.Fatalf("normalizeOne: %v", err)
	}
	if ok.Name != "Nightly" || ok.Overlap != "skip" || ok.Enabled == nil || !*ok.Enabled {
		t.Fatalf("defaults not applied: %+v", ok)
	}

	at := baseDef("at-job")
	at.Every = ""
	at.At = "03:30"
	got, err := normalizeOne(at)
	if err != nil {
		t.Fatalf("at schedule: %v", err)
	}
	if got.Every != "24h0m0s" {
		t.Errorf("at without every = %q, want 24h0m0s", got.Every)
	}

	bad := []Definition{
		{ID: "no-target", Name: "x", QuickAction: "qa", Every: "1h"},
		{ID: "both", Name: "x", Targets: []string{"a"}, QuickAction: "qa", Command: "true", Every: "1h"},
		{ID: "bad-cadence", Name: "x", Targets: []string{"a"}, QuickAction: "qa"},
		{ID: "Bad_ID", Name: "x", Targets: []string{"a"}, QuickAction: "qa", Every: "1h"},
		{ID: "bad-at", Name: "x", Targets: []string{"a"}, QuickAction: "qa", At: "3pm"},
	}
	for _, def := range bad {
		if _, err := normalizeOne(def); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", def.ID, err)
		}
	}
}

func TestNextDueIntervalAndAt(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	c, _ := compile(baseDef("nightly"))

	st := &ScheduleState{LastScheduled: base.Add(-time.Hour).Unix()}
	if got := s.nextDue(c, st, base); !got.Equal(base) {
		t.Errorf("interval next = %v, want %v", got, base)
	}

	atDef := baseDef("at-job")
	atDef.Every = ""
	atDef.At = "03:30"
	atC, _ := compile(atDef)
	// A new at-schedule before today's time runs today.
	early := time.Date(2026, 10, 19, 3, 0, 0, 0, time.Local)
	if got := s.nextDue(atC, &ScheduleState{}, early); got.Hour() != 3 || got.Minute() != 30 || got.Day() != early.Day() {
		t.Errorf("at next = %v", got)
	}
	// After today's time it moves to tomorrow.
	late := time.Date(2026, 10, 19, 4, 0, 0, 0, time.Local)
	if got := s.nextDue(atC, &ScheduleState{}, late); got.Day() != late.AddDate(0, 0, 1).Day() {
		t.Errorf("late at next = %v", got)
	}
}

func TestTickRunsDueSchedule(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	s.mu.Lock()
	s.stateForLocked("nightly").LastScheduled = base.Add(-time.Hour).Unix()
	s.mu.Unlock()

	s.tick(base)
	waitFor(t, "scheduled job", func() bool { return env.startCount() == 1 })

	view, _ := s.Get("nightly")
	if view.LastStatus != "success" || len(view.History) != 1 || view.History[0].Trigger != "schedule" {
		t.Fatalf("view = %+v", view)
	}
	if view.History[0].Targets[0].Status != "started" || view.History[0].Targets[0].JobID == "" {
		t.Fatalf("target run = %+v", view.History[0].Targets[0])
	}
	if !time.Unix(view.NextRun, 0).After(base) {
		t.Errorf("next run %v is not in the future", time.Unix(view.NextRun, 0))
	}
}

func TestTickSkipsMissedRuns(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	s.mu.Lock()
	s.stateForLocked("nightly").LastScheduled = base.Add(-2*time.Hour - 10*time.Minute).Unix()
	s.mu.Unlock()

	s.tick(base)
	// Allow any accidental goroutine to run before asserting.
	time.Sleep(20 * time.Millisecond)
	if got := env.startCount(); got != 0 {
		t.Fatalf("missed schedule started %d jobs, want 0", got)
	}
	view, _ := s.Get("nightly")
	if view.LastStatus != "missed" {
		t.Fatalf("last status = %q, want missed", view.LastStatus)
	}
	if len(view.History) != 1 || view.History[0].Status != "missed" {
		t.Fatalf("history = %+v", view.History)
	}
	if !strings.Contains(view.LastSummary, "2 missed") {
		t.Errorf("summary = %q", view.LastSummary)
	}
}

func TestRunNowIsManualAndBypassesOverlap(t *testing.T) {
	env := &fakeEnv{
		targets:  []Target{testTarget()},
		statuses: map[string]string{"web-1/old-job": jobs.StatusRunning},
	}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	s.mu.Lock()
	st := s.stateForLocked("nightly")
	st.LastScheduled = base.Add(-time.Hour).Unix()
	st.LastJobs = map[string]string{"web-1": "old-job"}
	s.mu.Unlock()

	// A scheduled run with the previous job still running is skipped.
	s.runTargets(context.Background(), baseDef("nightly"), mustCompile(t, baseDef("nightly")), false)
	if env.startCount() != 0 {
		t.Fatalf("overlap: started a duplicate job")
	}

	// Run now bypasses the overlap policy.
	run, err := s.RunNow(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if run.Trigger != "manual" || run.Status != "success" || len(run.Targets) != 1 {
		t.Fatalf("run = %+v", run)
	}
	if env.startCount() != 1 {
		t.Fatalf("manual run did not start a job")
	}
}

func TestScheduledOverlapSkip(t *testing.T) {
	env := &fakeEnv{
		targets:  []Target{testTarget()},
		statuses: map[string]string{"web-1/old-job": jobs.StatusRunning},
	}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	s.mu.Lock()
	st := s.stateForLocked("nightly")
	st.LastJobs = map[string]string{"web-1": "old-job"}
	s.mu.Unlock()

	results := s.runTargets(context.Background(), baseDef("nightly"), mustCompile(t, baseDef("nightly")), false)
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("results = %+v", results)
	}
	if env.startCount() != 0 {
		t.Fatalf("overlap started a job")
	}
}

func TestResolveTargetsUnion(t *testing.T) {
	env := &fakeEnv{targets: []Target{
		{ID: "b", Tags: []string{"prod"}},
		{ID: "a", Tags: []string{"prod"}},
		{ID: "c"},
	}}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	def := baseDef("nightly")
	def.Targets = []string{"a", "missing"}
	def.Tags = []string{"prod"}

	targets, missing := s.resolveTargets(def)
	if len(targets) != 2 || targets[0].ID != "a" || targets[1].ID != "b" {
		t.Fatalf("targets = %+v", targets)
	}
	if len(missing) != 1 || missing[0] != "missing" {
		t.Fatalf("missing = %v", missing)
	}
}

func TestCreateUpdateDeletePersists(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	s, dir := newTestScheduler(t, nil, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }

	view, err := s.Create(baseDef("nightly"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if view.Name != "Nightly" || view.NextRun == 0 {
		t.Fatalf("view = %+v", view)
	}
	if _, err := s.Create(baseDef("nightly")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create error = %v", err)
	}

	updated := baseDef("nightly")
	updated.Name = "Renamed"
	updated.Every = "2h"
	view, err = s.Update("nightly", updated)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if view.Name != "Renamed" || view.Every != "2h0m0s" {
		t.Fatalf("updated view = %+v", view)
	}

	defPath := filepath.Join(dir, "schedules.conf")
	if info, err := os.Stat(defPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("schedules.conf mode: %v %v", info, err)
	}
	reloaded, warnings := LoadDefinitions(defPath)
	if len(warnings) != 0 || len(reloaded) != 1 || reloaded[0].Name != "Renamed" {
		t.Fatalf("reloaded = %+v, warnings = %v", reloaded, warnings)
	}

	if err := s.Delete("nightly"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get("nightly"); ok {
		t.Fatal("schedule still present after delete")
	}
	if err := s.Delete("nightly"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete error = %v", err)
	}
}

func TestTimeoutWrappedCommand(t *testing.T) {
	got := withTimeout("echo hi", 30*time.Minute)
	if !strings.Contains(got, "timeout 1800 sh -c") || !strings.Contains(got, "echo hi") {
		t.Fatalf("withTimeout = %q", got)
	}
	if withTimeout("echo hi", 0) != "echo hi" {
		t.Fatal("zero timeout should not wrap")
	}
}

func mustCompile(t *testing.T, def Definition) compiled {
	t.Helper()
	c, err := compile(def)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func TestCronParseAndNext(t *testing.T) {
	c, err := parseCron("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatalf("parseCron: %v", err)
	}
	base := time.Date(2026, 10, 19, 8, 7, 0, 0, time.Local) // Monday
	next := c.next(base)
	if next.Hour() != 9 || next.Minute() != 0 {
		t.Fatalf("next = %v, want 09:00", next)
	}
	next = c.next(next)
	if next.Hour() != 9 || next.Minute() != 15 {
		t.Fatalf("second next = %v, want 09:15", next)
	}

	bad := []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "a * * * *"}
	for _, expr := range bad {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) should fail", expr)
		}
	}
}

func TestCronWeekdayAndDayOfMonth(t *testing.T) {
	c, err := parseCron("0 3 * * 1")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 19, 4, 0, 0, 0, time.Local) // Monday 04:00
	next := c.next(base)
	if next.Weekday() != time.Monday || next.Hour() != 3 {
		t.Fatalf("next = %v", next)
	}

	c, err = parseCron("0 3 1 * *")
	if err != nil {
		t.Fatal(err)
	}
	next = c.next(base)
	if next.Day() != 1 || next.Hour() != 3 {
		t.Fatalf("dom next = %v", next)
	}
}

func TestNormalizeCronClearsInterval(t *testing.T) {
	def := baseDef("cron-job")
	def.Cron = "0 3 * * *"
	def.Every = "1h"
	def.At = "03:00"
	got, err := normalizeOne(def)
	if err != nil {
		t.Fatalf("normalizeOne: %v", err)
	}
	if got.Cron != "0 3 * * *" || got.Every != "" || got.At != "" {
		t.Fatalf("normalized = %+v", got)
	}

	def.Cron = "bad cron"
	if _, err := normalizeOne(def); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cron error = %v", err)
	}
}

func TestNextDueCron(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("cron-job")
	def.Cron = "*/10 * * * *"
	def.Every = ""
	s, _ := newTestScheduler(t, []Definition{def}, env)
	base := time.Date(2026, 10, 19, 12, 3, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	c := mustCompile(t, def)
	st := &ScheduleState{LastScheduled: base.Add(-time.Hour).Unix()}
	got := s.nextDue(c, st, base)
	if got.Minute()%10 != 0 || !got.After(base.Add(-time.Hour)) {
		t.Fatalf("next due = %v", got)
	}
}

func TestOneOffSchedule(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("once")
	def.Every = ""
	def.Date = "2026-10-25"
	def.At = "09:00"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }

	s.mu.Lock()
	s.stateForLocked("once").CreatedAt = base.Unix()
	s.mu.Unlock()
	view, _ := s.Get("once")
	want := time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local).Unix()
	if view.NextRun != want || len(view.NextRuns) != 1 || view.NextRuns[0] != want {
		t.Fatalf("view = %+v, want next %d", view, want)
	}

	runs := s.Occurrences(time.Date(2026, 10, 25, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local))
	if len(runs) != 1 || runs[0].Time != want {
		t.Fatalf("occurrences = %+v", runs)
	}

	s.now = func() time.Time { return time.Unix(want, 0) }
	s.tick(time.Unix(want, 0))
	waitFor(t, "one-off job", func() bool { return env.startCount() == 1 })
	view, _ = s.Get("once")
	if len(view.NextRuns) != 0 {
		t.Fatalf("one-off still has next runs: %+v", view.NextRuns)
	}
}

func TestWeekdaySchedule(t *testing.T) {
	def := baseDef("weekdays")
	def.Every = ""
	def.At = "09:30"
	def.Days = []string{"mon", "tue", "wed", "thu", "fri"}
	c := mustCompile(t, def)
	base := time.Date(2026, 10, 23, 10, 0, 0, 0, time.Local) // Friday after 09:30
	next := (&Scheduler{grace: defaultGrace}).nextDue(c, &ScheduleState{}, base)
	want := time.Date(2026, 10, 26, 9, 30, 0, 0, time.Local) // Monday
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestOccurrencesRespectCreatedAtAndRange(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("hourly")
	def.Every = "1h"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return start }
	s.mu.Lock()
	st := s.stateForLocked("hourly")
	st.CreatedAt = start.Unix()
	st.LastScheduled = start.Unix()
	s.mu.Unlock()
	from := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 19, 17, 0, 0, 0, time.Local)
	runs := s.Occurrences(from, to)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want 3", runs)
	}
	if runs[0].Time != from.Unix() || runs[2].Time != from.Add(2*time.Hour).Unix() {
		t.Fatalf("runs = %+v", runs)
	}
	// Nothing before the schedule was created.
	before := s.Occurrences(time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 19, 0, 0, 0, 0, time.Local))
	if len(before) != 0 {
		t.Fatalf("before creation = %+v", before)
	}
}

func TestPreviewReportsTargetsAndWarnings(t *testing.T) {
	env := &fakeEnv{targets: []Target{
		{ID: "web-1", QuickActions: []config.QuickAction{{ID: "restart", Label: "Restart", Command: "true"}}},
		{ID: "web-2"},
	}}
	s, _ := newTestScheduler(t, nil, env)
	def := Definition{
		Targets:     []string{"web-1", "web-2", "missing"},
		QuickAction: "restart",
		Days:        []string{"mon", "wed"},
		At:          "09:00",
	}
	preview, err := s.Preview(def)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(preview.NextRuns) != 5 {
		t.Fatalf("next runs = %+v", preview.NextRuns)
	}
	if len(preview.Targets) != 2 || len(preview.Missing) != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	if len(preview.Warnings) != 1 || !strings.Contains(preview.Warnings[0], "web-2") {
		t.Fatalf("warnings = %+v", preview.Warnings)
	}
}

func TestSetEnabledAndCadenceUpdateReset(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("nightly")
	s, _ := newTestScheduler(t, []Definition{def}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	st := s.stateForLocked("nightly")
	st.LastScheduled = base.Add(-30 * time.Hour).Unix()

	view, err := s.SetEnabled("nightly", false)
	if err != nil || view.Enabled == nil || *view.Enabled {
		t.Fatalf("disable = %+v, %v", view, err)
	}
	view, err = s.SetEnabled("nightly", true)
	if err != nil || view.Enabled == nil || !*view.Enabled {
		t.Fatalf("enable = %+v, %v", view, err)
	}
	if got := s.stateForLocked("nightly").LastScheduled; got != base.Unix() {
		t.Fatalf("enable LastScheduled = %d, want %d", got, base.Unix())
	}

	updated := def
	updated.Every = "2h"
	if _, err := s.Update("nightly", updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := s.stateForLocked("nightly").LastScheduled; got != base.Unix() {
		t.Fatalf("cadence update LastScheduled = %d, want %d", got, base.Unix())
	}
}

func TestLocalTargetAllowed(t *testing.T) {
	def := Definition{
		ID:      "local",
		Name:    "local",
		Targets: []string{"__local__"},
		Command: "true",
		Every:   "1h",
	}
	if _, err := normalizeOne(def); err != nil {
		t.Fatalf("normalizeOne local target: %v", err)
	}
}

func TestDeleteOccurrence(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("hourly")
	def.Every = "1h"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return start }
	s.mu.Lock()
	st := s.stateForLocked("hourly")
	st.CreatedAt = start.Unix()
	st.LastScheduled = start.Unix()
	s.mu.Unlock()

	from := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 19, 17, 0, 0, 0, time.Local)
	runs := s.Occurrences(from, to)
	if len(runs) != 3 {
		t.Fatalf("initial runs = %+v", runs)
	}
	if _, err := s.DeleteOccurrence("hourly", runs[1].Time, false); err != nil {
		t.Fatalf("DeleteOccurrence one: %v", err)
	}
	runs = s.Occurrences(from, to)
	if len(runs) != 2 || runs[0].Time != from.Unix() || runs[1].Time != from.Add(2*time.Hour).Unix() {
		t.Fatalf("after one delete = %+v", runs)
	}
	if _, err := s.DeleteOccurrence("hourly", runs[1].Time, true); err != nil {
		t.Fatalf("DeleteOccurrence future: %v", err)
	}
	runs = s.Occurrences(from, to)
	if len(runs) != 1 || runs[0].Time != from.Unix() {
		t.Fatalf("after future delete = %+v", runs)
	}
	view, _ := s.Get("hourly")
	if view.EndsAt != from.Add(2*time.Hour).Unix() {
		t.Fatalf("EndsAt = %d", view.EndsAt)
	}
}

func TestIntervalDays(t *testing.T) {
	for every, want := range map[string]int{
		"24h":   1,
		"48h":   2,
		"72h":   3,
		"1h30m": 0,
		"23h":   0,
		"25h":   0,
		"0s":    0,
	} {
		d, err := time.ParseDuration(every)
		if err != nil {
			t.Fatalf("parse %s: %v", every, err)
		}
		if got := intervalDays(d); got != want {
			t.Errorf("intervalDays(%s) = %d, want %d", every, got, want)
		}
	}
}

// A whole-day interval ("every 24h") must keep firing at the same local time
// across DST shifts instead of drifting an hour per transition.
func TestNextDueDayIntervalSurvivesDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("daily")
	def.Every = "24h"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	c, _ := compile(def)

	// Last run 09:30 EST on 2025-03-08; the US spring-forward happened that
	// night (02:00 -> 03:00). The next occurrence should be 09:30 wall clock
	// on 03-09 (only 23 absolute hours later), not 10:30.
	last := time.Date(2025, 3, 8, 9, 30, 0, 0, loc)
	now := time.Date(2025, 3, 8, 10, 0, 0, 0, loc)
	st := &ScheduleState{LastScheduled: last.Unix()}
	got := s.nextDueRaw(c, st, now)
	want := time.Date(2025, 3, 9, 9, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("next due = %v, want %v (wall-clock day step across DST)", got, want)
	}

	// Fall-back on 2025-11-02: 09:30 EDT on 11-01 steps to 09:30 EST, not the
	// absolute +24h instant (10:30 wall).
	last = time.Date(2025, 11, 1, 9, 30, 0, 0, loc)
	now = time.Date(2025, 11, 1, 10, 0, 0, 0, loc)
	st = &ScheduleState{LastScheduled: last.Unix()}
	got = s.nextDueRaw(c, st, now)
	want = time.Date(2025, 11, 2, 9, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("next due = %v, want %v (fall-back wall-clock step)", got, want)
	}
}

func TestOccurrencesDayIntervalDSTStable(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	def := baseDef("daily")
	def.Every = "24h"
	c, _ := compile(def)
	created := time.Date(2025, 3, 7, 9, 30, 0, 0, loc)
	st := &ScheduleState{CreatedAt: created.Unix(), LastScheduled: created.Unix()}
	from := time.Date(2025, 3, 8, 0, 0, 0, 0, loc)
	to := time.Date(2025, 3, 12, 0, 0, 0, 0, loc)
	got := occurrenceTimes(def, c, st, from, to, 0)
	if len(got) != 4 {
		t.Fatalf("expansion = %v (%d runs), want 4", got, len(got))
	}
	for _, ts := range got {
		d := time.Unix(ts, 0).In(loc)
		if d.Hour() != 9 || d.Minute() != 30 {
			t.Errorf("occurrence %d at %s, want 09:30", ts, d.Format("2006-01-02 15:04"))
		}
	}
}

func TestLoadDefinitionsSkipsInvalidEntries(t *testing.T) {
	conf := `schedules:
  - name: Good
    targets: ["web-1"]
    quick_action: restart
    every: 1h
  - name: Bad
    targets: ["web-1"]
    every: 1h
  - name: Good
    targets: ["web-1"]
    quick_action: restart
    every: 1h
  - name: Also Bad
    targets: ["web-1"]
    quick_action: restart
    at: "3pm"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	defs, warnings := LoadDefinitions(path)
	if len(defs) != 1 || defs[0].Name != "Good" {
		t.Fatalf("defs = %+v, want only the good entry", defs)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %+v, want 3 skips (invalid, duplicate, invalid)", warnings)
	}
}

func TestLoadDefinitionsCorruptFileIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.conf")
	if err := os.WriteFile(path, []byte("schedules: [ this is: not: valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	defs, warnings := LoadDefinitions(path)
	if len(defs) != 0 {
		t.Fatalf("defs = %+v, want none", defs)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want 1 parse warning", warnings)
	}
}

func TestJobFinishedFoldsExitStatusIntoHistory(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	run, err := s.RunNow(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	jobID := run.Targets[0].JobID
	if jobID == "" {
		t.Fatal("no job id recorded")
	}

	zero := 0
	s.JobFinished("web-1", jobID, jobs.StatusExited, &zero)
	view, ok := s.Get("nightly")
	if !ok {
		t.Fatal("schedule missing")
	}
	tr := view.History[0].Targets[0]
	if tr.Status != "done" || view.History[0].Status != "success" || view.LastStatus != "success" {
		t.Fatalf("after exit 0: run=%+v status=%q summary=%q", view.History[0], view.LastStatus, view.LastSummary)
	}
	if tr.Error != "" {
		t.Fatalf("unexpected error text: %q", tr.Error)
	}

	// A second run ends non-zero: headline flips to an error state.
	s.now = func() time.Time { return base.Add(time.Hour) }
	code := 3
	run, err = s.RunNow(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("RunNow 2: %v", err)
	}
	s.JobFinished("web-1", run.Targets[0].JobID, jobs.StatusExited, &code)
	view, _ = s.Get("nightly")
	if view.History[0].Targets[0].Status != "failed" || view.LastStatus != "error" {
		t.Fatalf("after exit 3: target=%q headline=%q", view.History[0].Targets[0].Status, view.LastStatus)
	}
	if view.History[0].Targets[0].Error != "exit code 3" {
		t.Fatalf("error text = %q", view.History[0].Targets[0].Error)
	}

	// An unrelated job id changes nothing.
	s.JobFinished("web-1", "unrelated", jobs.StatusExited, &zero)
	view, _ = s.Get("nightly")
	if view.LastStatus != "error" {
		t.Fatalf("unrelated job changed status to %q", view.LastStatus)
	}
}

func TestSkippedOccurrencesCapped(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("hourly")
	def.Every = "1h"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }

	for i := 0; i < maxSkipped+10; i++ {
		if _, err := s.DeleteOccurrence("hourly", base.AddDate(0, 0, i).Unix(), false); err != nil {
			t.Fatalf("DeleteOccurrence %d: %v", i, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := len(s.state.Schedules["hourly"].Skipped); got != maxSkipped {
		t.Fatalf("skipped list length = %d, want %d", got, maxSkipped)
	}
}

func TestSetEnabledRecordsRunsSkippedWhilePaused(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	def := baseDef("nightly")
	def.Every = "1h"
	s, _ := newTestScheduler(t, []Definition{def}, env)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return base }
	s.mu.Lock()
	st := s.stateForLocked("nightly")
	st.CreatedAt = base.Add(-4 * time.Hour).Unix()
	st.LastScheduled = base.Add(-4 * time.Hour).Unix()
	s.mu.Unlock()

	if _, err := s.SetEnabled("nightly", false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := s.SetEnabled("nightly", true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	view, _ := s.Get("nightly")
	if len(view.History) != 1 || view.History[0].Trigger != "pause" || view.History[0].Status != "skipped" {
		t.Fatalf("history = %+v", view.History)
	}
	if !strings.Contains(view.History[0].Summary, "3 run(s) skipped while paused") {
		t.Fatalf("summary = %q", view.History[0].Summary)
	}
	// Resuming re-baselines: no immediate new run.
	if got := view.NextRun; got == 0 || time.Unix(got, 0).Before(base) {
		t.Fatalf("next run = %v", time.Unix(got, 0))
	}
}

// The acute watcher must fold a prompt completion into history via the
// registry's change hook, without waiting for the periodic reconciler.
func TestWatcherFoldsQuickCompletion(t *testing.T) {
	env := &fakeEnv{targets: []Target{testTarget()}}
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	s, _ := newTestScheduler(t, []Definition{baseDef("nightly")}, env)
	s.now = func() time.Time { return base }
	s.watchInterval = 20 * time.Millisecond

	// Polls first see the job running, then it finishes with exit code 4 and
	// the change hook (registered on the hub in production) folds it in.
	// The status hook is installed before RunNow so the watcher goroutine
	// never races the test writing it; it reads the job id from its arguments.
	calls := 0
	folded := false
	env.statusFn = func(serverID, jobID string) (string, bool) {
		if jobID != "" {
			calls++
			if calls >= 2 && !folded {
				folded = true
				code := 4
				s.JobFinished(serverID, jobID, jobs.StatusExited, &code)
				return "exited", true
			}
		}
		return "running", true
	}

	run, err := s.RunNow(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	jobID := run.Targets[0].JobID
	if jobID == "" {
		t.Fatal("no job id recorded")
	}

	waitFor(t, "folded history", func() bool {
		view, ok := s.Get("nightly")
		return ok && view.LastStatus == "error"
	})
	view, _ := s.Get("nightly")
	tr := view.History[0].Targets[0]
	if tr.Status != "failed" || tr.JobID != jobID {
		t.Fatalf("target = %+v", tr)
	}
}

// A cron schedule must not re-fire during a fall-back's repeated hour.
func TestCronSkipsRepeatedDSTHour(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zone data unavailable: %v", err)
	}
	spec, err := parseCron("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}

	// First pass of 01:30 on 2025-11-02 (time.Date picks the first instant
	// for an ambiguous local time). The second 01:30 must not match.
	first := time.Date(2025, 11, 2, 1, 30, 0, 0, loc)
	got := spec.next(first)
	want := time.Date(2025, 11, 3, 1, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("next after first 01:30 = %v, want %v (no double fire)", got, want)
	}

	// Sanity: the normal path is untouched.
	normal := time.Date(2025, 11, 3, 5, 0, 0, 0, loc)
	if got := spec.next(normal); !got.Equal(time.Date(2025, 11, 4, 1, 30, 0, 0, loc)) {
		t.Errorf("next after 05:00 = %v", got)
	}
}
