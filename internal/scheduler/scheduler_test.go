package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

type mockExec struct {
	mu                                    sync.Mutex
	started, stopped, restarted, backedup int
	commands                              []string
	seq                                   []string
	fail                                  map[models.ScheduleAction]bool
}

func (m *mockExec) do(a models.ScheduleAction, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq = append(m.seq, string(a))
	switch a {
	case models.SchedStart:
		m.started++
	case models.SchedStop:
		m.stopped++
	case models.SchedRestart:
		m.restarted++
	case models.SchedBackup:
		m.backedup++
	case models.SchedCommand:
		m.commands = append(m.commands, payload)
	}
	if m.fail[a] {
		return fmt.Errorf("boom")
	}
	return nil
}

func (m *mockExec) Start(context.Context, *models.Server) error { return m.do(models.SchedStart, "") }
func (m *mockExec) Stop(context.Context, *models.Server) error  { return m.do(models.SchedStop, "") }
func (m *mockExec) Restart(context.Context, *models.Server) error {
	return m.do(models.SchedRestart, "")
}
func (m *mockExec) Backup(context.Context, *models.Server) error { return m.do(models.SchedBackup, "") }
func (m *mockExec) Command(_ context.Context, _ *models.Server, c string) error {
	return m.do(models.SchedCommand, c)
}

func (m *mockExec) sequence() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seq...)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "s.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// runTick fires the scheduler once and waits for any async chains to finish so
// assertions are deterministic.
func runTick(s *Scheduler, ctx context.Context) {
	s.Tick(ctx)
	s.Wait()
}

func TestTickFiresDueSchedule(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	// Legacy single-action schedule (no Tasks): exercises TaskChain normalization.
	sc := &models.Schedule{ServerID: srv.ID, Name: "wake", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &past}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	m := &mockExec{}
	runTick(New(st, m), context.Background())

	if m.started != 1 {
		t.Fatalf("Start called %d times, want 1", m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if got.LastRun == nil {
		t.Error("LastRun not recorded")
	}
	if got.NextRun == nil || !got.NextRun.After(time.Now()) {
		t.Errorf("NextRun not advanced into the future: %v", got.NextRun)
	}
	if !strings.Contains(got.LastStatus, "ok") {
		t.Errorf("LastStatus = %q, want it to contain ok", got.LastStatus)
	}
}

func TestTickComputesMissingNextRunWithoutFiring(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns"}
	_ = st.CreateServer(srv)
	// No NextRun set: scheduler should compute one and NOT fire retroactively.
	sc := &models.Schedule{ServerID: srv.ID, Name: "nightly", Cron: "0 4 * * *", Action: models.SchedStop, Enabled: true}
	_ = st.CreateSchedule(sc)

	m := &mockExec{}
	runTick(New(st, m), context.Background())

	if m.stopped != 0 {
		t.Errorf("should not fire on first sighting, Stop called %d", m.stopped)
	}
	got, _ := st.GetSchedule(sc.ID)
	if got.NextRun == nil {
		t.Error("NextRun should have been computed")
	}
}

func TestTickIgnoresDisabledAndFuture(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns"}
	_ = st.CreateServer(srv)
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	_ = st.CreateSchedule(&models.Schedule{ServerID: srv.ID, Name: "off", Cron: "* * * * *", Action: models.SchedStart, Enabled: false, NextRun: &past})
	_ = st.CreateSchedule(&models.Schedule{ServerID: srv.ID, Name: "later", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &future})

	m := &mockExec{}
	runTick(New(st, m), context.Background())
	if m.started != 0 {
		t.Errorf("no schedule should have fired, Start called %d", m.started)
	}
}

func TestTickSkipsPowerActionsOnSuspendedServer(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateSuspended}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Name: "wake", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &past}
	_ = st.CreateSchedule(sc)

	m := &mockExec{}
	runTick(New(st, m), context.Background())

	if m.started != 0 {
		t.Errorf("Start fired on a suspended server (%d); suspension must hold", m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "skipped (server suspended)") {
		t.Errorf("LastStatus = %q, want it to mention skipped (server suspended)", got.LastStatus)
	}
}

func TestChainRunsTasksInOrder(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "graceful", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedCommand, Payload: "say restarting"},
			{Action: models.SchedStop, TimeOffset: 10},
			{Action: models.SchedBackup, TimeOffset: 5},
			{Action: models.SchedStart},
		},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{}
	var slept []time.Duration
	s := New(st, m)
	s.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	runTick(s, context.Background())

	want := []string{"command", "stop", "backup", "start"}
	if got := m.sequence(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("task order = %v, want %v", got, want)
	}
	if len(slept) != 2 || slept[0] != 10*time.Second || slept[1] != 5*time.Second {
		t.Errorf("delays = %v, want [10s 5s]", slept)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "#4 start: ok") {
		t.Errorf("status = %q, want it to include #4 start: ok", got.LastStatus)
	}
}

func TestChainAbortsOnFailure(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedStop},   // fails
			{Action: models.SchedBackup}, // must NOT run (chain aborts)
		},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{fail: map[models.ScheduleAction]bool{models.SchedStop: true}}
	runTick(New(st, m), context.Background())

	if m.stopped != 1 || m.backedup != 0 {
		t.Errorf("aborted chain ran wrong tasks: stopped=%d backedup=%d", m.stopped, m.backedup)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "chain aborted") {
		t.Errorf("status = %q, want it to mention chain aborted", got.LastStatus)
	}
}

func TestChainContinueOnFailure(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedBackup, ContinueOnFailure: true}, // fails but chain continues
			{Action: models.SchedStart},                           // still runs
		},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{fail: map[models.ScheduleAction]bool{models.SchedBackup: true}}
	runTick(New(st, m), context.Background())

	if m.backedup != 1 || m.started != 1 {
		t.Errorf("continue-on-failure chain: backedup=%d started=%d, want 1/1", m.backedup, m.started)
	}
}

func TestChainCancelledByContext(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedStop},                  // runs (offset 0)
			{Action: models.SchedStart, TimeOffset: 30}, // delay aborts on cancel
		},
	}
	_ = st.CreateSchedule(sc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &mockExec{}
	s := New(st, m)
	s.Sleep = func(c context.Context, _ time.Duration) error { return c.Err() }
	runTick(s, ctx)

	if m.stopped != 1 || m.started != 0 {
		t.Errorf("cancelled chain: stopped=%d started=%d, want 1/0", m.stopped, m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "cancelled") {
		t.Errorf("status = %q, want it to mention cancelled", got.LastStatus)
	}
}

// TestNoOverlap verifies the in-flight guard: while a chain is mid-delay, a
// second due tick must not start it again.
func TestNoOverlap(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedStop},                 // runs immediately
			{Action: models.SchedStart, TimeOffset: 1}, // blocks in Sleep until released
		},
	}
	_ = st.CreateSchedule(sc)

	block := make(chan struct{})
	m := &mockExec{}
	s := New(st, m)
	s.Sleep = func(_ context.Context, _ time.Duration) error { <-block; return nil }

	s.Tick(context.Background()) // launches the chain; it acquires the in-flight lock
	// Force the schedule due again while the first run is blocked.
	_ = st.SetScheduleNextRun(sc.ID, &past)
	s.Tick(context.Background()) // must be skipped by the in-flight guard
	close(block)
	s.Wait()

	if m.stopped != 1 || m.started != 1 {
		t.Errorf("overlap: stopped=%d started=%d, want 1/1 (guard should prevent a second run)", m.stopped, m.started)
	}
}

func TestTickRemovesOrphanSchedule(t *testing.T) {
	st := testStore(t)
	past := time.Now().Add(-time.Minute)
	// Schedule points at a non-existent server.
	sc := &models.Schedule{ServerID: 9999, Name: "orphan", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &past}
	_ = st.CreateSchedule(sc)

	runTick(New(st, &mockExec{}), context.Background())

	if _, err := st.GetSchedule(sc.ID); err == nil {
		t.Error("orphan schedule should have been deleted")
	}
}

func TestNextRunRejectsBadCron(t *testing.T) {
	if _, err := NextRun("not a cron", time.Now()); err == nil {
		t.Error("expected error for invalid cron")
	}
	if _, err := NextRun("*/5 * * * *", time.Now()); err != nil {
		t.Errorf("valid cron rejected: %v", err)
	}
}
