package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
)

type mockExec struct {
	mu                                    sync.Mutex
	started, stopped, restarted, backedup int
	commands                              []string
	seq                                   []string
	fail                                  map[models.ScheduleAction]bool
	// st receives the backups the chain asks for. They are over at once unless
	// pending is set, for a test to watch the chain wait for one.
	st      *store.Store
	pending bool
	backups []uint
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
func (m *mockExec) Backup(_ context.Context, srv *models.Server) (uint, error) {
	if err := m.do(models.SchedBackup, ""); err != nil {
		return 0, err
	}
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded}
	if m.pending {
		b.Phase = models.BackupPending
	}
	if err := m.st.CreateBackup(b); err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.backups = append(m.backups, b.ID)
	m.mu.Unlock()
	return b.ID, nil
}
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
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "s.db"), Silent: true})
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

	m := &mockExec{st: st}
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

	m := &mockExec{st: st}
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

	m := &mockExec{st: st}
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

	m := &mockExec{st: st}
	runTick(New(st, m), context.Background())

	if m.started != 0 {
		t.Errorf("Start fired on a suspended server (%d); suspension must hold", m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "skipped (server suspended)") {
		t.Errorf("LastStatus = %q, want it to mention skipped (server suspended)", got.LastStatus)
	}
}

// A suspended server is frozen until an administrator looks into it. Its
// scheduled backups used to go on, and each one's retention pushed out a
// snapshot from before the suspension: a nightly schedule emptied the history
// in a week.
func TestTickRunsNothingOnSuspendedServer(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateSuspended}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "nightly", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedCommand, Payload: "save-all"},
			{Action: models.SchedBackup},
		},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{st: st}
	runTick(New(st, m), context.Background())

	if m.backedup != 0 || len(m.commands) != 0 {
		t.Errorf("on a suspended server: %d backups, commands %q; want none", m.backedup, m.commands)
	}
	got, _ := st.GetSchedule(sc.ID)
	if strings.Count(got.LastStatus, "skipped (server suspended)") != 2 {
		t.Errorf("LastStatus = %q, want both tasks skipped (server suspended)", got.LastStatus)
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

	m := &mockExec{st: st}
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

	m := &mockExec{st: st, fail: map[models.ScheduleAction]bool{models.SchedStop: true}}
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

	m := &mockExec{st: st, fail: map[models.ScheduleAction]bool{models.SchedBackup: true}}
	runTick(New(st, m), context.Background())

	if m.backedup != 1 || m.started != 1 {
		t.Errorf("continue-on-failure chain: backedup=%d started=%d, want 1/1", m.backedup, m.started)
	}
}

// A controller that goes away mid-chain -- an upgrade, a rollout, a node
// drain -- used to drop the rest of it: the recette of 0.10.0 restarted the
// controller between the two steps of a chain, and the second never ran
// (R-20). The chain is left where it was, and the next controller carries it
// on, its delay kept.
func TestAChainLeftHalfDoneIsCarriedOn(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{
			{Action: models.SchedStop},                  // runs (offset 0)
			{Action: models.SchedStart, TimeOffset: 30}, // the controller goes away during its delay
		},
	}
	_ = st.CreateSchedule(sc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &mockExec{st: st}
	s := New(st, m)
	s.Sleep = func(c context.Context, _ time.Duration) error { return c.Err() }
	runTick(s, ctx)
	if m.stopped != 1 || m.started != 0 {
		t.Fatalf("before the restart: stopped=%d started=%d, want 1/0", m.stopped, m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if got.Run == nil || got.Run.Next != 1 {
		t.Fatalf("the chain's progress was not kept: %+v", got.Run)
	}

	// The next controller.
	var slept []time.Duration
	next := New(st, m)
	next.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	runTick(next, context.Background())
	if m.started != 1 || m.stopped != 1 {
		t.Errorf("after the restart: stopped=%d started=%d, want 1/1", m.stopped, m.started)
	}
	if len(slept) != 1 || slept[0] <= 0 || slept[0] > 30*time.Second {
		t.Errorf("waited %v before the start, want what was left of its 30s", slept)
	}
	got, _ = st.GetSchedule(sc.ID)
	if got.Run != nil || !strings.Contains(got.LastStatus, "#2 start: ok") || !strings.Contains(got.LastStatus, "carried on") {
		t.Errorf("run %+v, status %q: want the chain ended, both steps told", got.Run, got.LastStatus)
	}
}

// A backup step carried on waits for the backup it asked for, rather than
// taking another one.
func TestACarriedOnBackupStepWaitsForItsBackup(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedStart}},
	}
	_ = st.CreateSchedule(sc)

	ctx, cancel := context.WithCancel(context.Background())
	m := &mockExec{st: st, pending: true}
	s := New(st, m)
	s.Sleep = func(c context.Context, _ time.Duration) error { cancel(); return c.Err() }
	runTick(s, ctx)
	got, _ := st.GetSchedule(sc.ID)
	if got.Run == nil || got.Run.Backup == 0 {
		t.Fatalf("the backup the step waits for was not kept: %+v", got.Run)
	}
	b, _ := st.GetBackup(got.Run.Backup)
	b.Phase = models.BackupSucceeded
	_ = st.UpdateBackup(b)

	next := New(st, m)
	next.Sleep = func(context.Context, time.Duration) error { return nil }
	runTick(next, context.Background())
	if m.backedup != 1 || m.started != 1 {
		t.Errorf("backups taken %d, starts %d: want the one backup waited for, then the start", m.backedup, m.started)
	}
}

// A chain left far too long is not replayed.
func TestAChainLeftADayAgoIsNotCarriedOn(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	future := time.Now().Add(time.Hour)
	stale := time.Now().Add(-resumeLimit - time.Hour)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "x", Cron: "* * * * *", Enabled: true, NextRun: &future,
		Tasks: []models.ScheduleTask{{Action: models.SchedStop}, {Action: models.SchedStart}},
		Run:   &models.ScheduleRun{Fired: stale, Next: 1, Due: stale, Done: []string{"#1 stop: ok"}},
	}
	_ = st.CreateSchedule(sc)
	m := &mockExec{st: st}
	runTick(New(st, m), context.Background())
	got, _ := st.GetSchedule(sc.ID)
	if m.started != 0 || got.Run != nil || !strings.Contains(got.LastStatus, "not run") {
		t.Errorf("started %d, run %+v, status %q: want it closed without running", m.started, got.Run, got.LastStatus)
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
	m := &mockExec{st: st}
	s := New(st, m)
	s.Sleep = func(_ context.Context, _ time.Duration) error { <-block; return nil }

	s.Tick(context.Background()) // launches the chain; it acquires the in-flight lock
	s.Tick(context.Background()) // must be skipped by the in-flight guard
	close(block)
	s.Wait()

	if m.stopped != 1 || m.started != 1 {
		t.Errorf("overlap: stopped=%d started=%d, want 1/1 (guard should prevent a second run)", m.stopped, m.started)
	}
}

// panicExec panics on the configured action to exercise the goroutine's panic
// guard (a detached chain must not crash the controller).
type panicExec struct{ mockExec }

func (p *panicExec) Stop(context.Context, *models.Server) error { panic("kaboom") }

func TestChainPanicIsRecovered(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Name: "boom", Cron: "* * * * *", Action: models.SchedStop, Enabled: true, NextRun: &past}
	_ = st.CreateSchedule(sc)

	// If the panic escaped the goroutine, the test process would crash.
	runTick(New(st, &panicExec{}), context.Background())

	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "panic") {
		t.Errorf("status = %q, want it to record the panic", got.LastStatus)
	}
}

func TestTickRemovesOrphanSchedule(t *testing.T) {
	st := testStore(t)
	past := time.Now().Add(-time.Minute)
	// Schedule points at a non-existent server.
	sc := &models.Schedule{ServerID: 9999, Name: "orphan", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &past}
	_ = st.CreateSchedule(sc)

	runTick(New(st, &mockExec{st: st}), context.Background())

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

// A scheduled start during a cross-cluster transfer would put a pod back on the
// volume the transfer is waiting to have to itself, and the transfer would wait
// for good. The API refuses it; the scheduler must too.
func TestScheduledStartSkippedDuringTransfer(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateStopped}
	_ = st.CreateServer(srv)
	if _, err := st.BeginServerTransfer(srv.ID, 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Name: "morning", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedStart}}}
	_ = st.CreateSchedule(sc)

	m := &mockExec{st: st}
	runTick(New(st, m), context.Background())

	if m.started != 0 {
		t.Errorf("Start fired during a transfer (%d)", m.started)
	}
	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "transferred") {
		t.Errorf("LastStatus = %q", got.LastStatus)
	}
}

// "Stop, back up, start" started the server again the moment the backup was
// requested, so the copy was taken of a running game; a chain that paused the
// game's saves for a backup resumed them before anything was copied. A backup
// step now ends when its backup does.
func TestABackupStepWaitsForItsBackup(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "cold backup", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedStop}, {Action: models.SchedBackup}, {Action: models.SchedStart}},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{st: st, pending: true}
	s := New(st, m)
	polls := 0
	s.Sleep = func(_ context.Context, d time.Duration) error {
		polls++
		if got := strings.Join(m.sequence(), ","); got != "stop,backup" {
			t.Errorf("while the backup runs, the chain has done %s", got)
		}
		if polls == 3 {
			b, _ := st.GetBackup(m.backups[0])
			b.Phase = models.BackupSucceeded
			_ = st.UpdateBackup(b)
		}
		return nil
	}
	runTick(s, context.Background())

	if got := strings.Join(m.sequence(), ","); got != "stop,backup,start" || polls != 3 {
		t.Errorf("sequence %s after %d polls, want stop,backup,start once the backup was over", got, polls)
	}
	if got, _ := st.GetSchedule(sc.ID); !strings.Contains(got.LastStatus, "#2 backup: ok; #3 start: ok") {
		t.Errorf("status = %q", got.LastStatus)
	}
}

// The step fails with its backup, which a nightly backup schedule used to
// report as "ok" whatever became of it.
func TestABackupStepFailsWithItsBackup(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "nightly", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedCommand, Payload: "say backed up"}},
	}
	_ = st.CreateSchedule(sc)

	m := &mockExec{st: st, pending: true}
	s := New(st, m)
	s.Sleep = func(context.Context, time.Duration) error {
		b, _ := st.GetBackup(m.backups[0])
		b.Phase, b.Message = models.BackupFailed, "Access Denied"
		_ = st.UpdateBackup(b)
		return nil
	}
	runTick(s, context.Background())

	got, _ := st.GetSchedule(sc.ID)
	if !strings.Contains(got.LastStatus, "#1 backup: error: the backup failed: Access Denied — chain aborted") || len(m.commands) != 0 {
		t.Errorf("status = %q, commands %q; want the backup's failure, and nothing after it", got.LastStatus, m.commands)
	}
}

// What a schedule did showed only as its last status, overwritten by the next
// run. Each run is now in the audit log and in the server's activity.
func TestAScheduleRunIsRecorded(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "survival", Namespace: "ns", DesiredState: models.StateRunning}
	_ = st.CreateServer(srv)
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{
		ServerID: srv.ID, Name: "nightly restart", Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedCommand, Payload: "say restarting"}, {Action: models.SchedRestart}},
	}
	_ = st.CreateSchedule(sc)
	runTick(New(st, &mockExec{st: st}), context.Background())

	want := `"nightly restart": #1 command: ok; #2 restart: ok`
	audit, err := st.ListAuditForServer(srv.ID, 0, 10)
	if err != nil || len(audit) != 1 || audit[0].Action != models.EventScheduleRun || audit[0].Detail != want || audit[0].UserID != 0 {
		t.Errorf("audit = %+v (%v), want one schedule.run entry %q by no user", audit, err, want)
	}
	events, err := st.ListEventsForServer(srv.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Type != models.EventScheduleRun || events[0].Message != "survival: "+want {
		t.Errorf("events = %+v (%v), want one schedule.run event", events, err)
	}
	if !models.KnownEventType(models.EventScheduleRun) {
		t.Error("schedule.run is not in the catalog a channel filters on")
	}
}
