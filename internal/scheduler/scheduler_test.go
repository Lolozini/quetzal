package scheduler

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

type mockExec struct {
	started, stopped, restarted, backedup int
	commands                              []string
}

func (m *mockExec) Start(context.Context, *models.Server) error   { m.started++; return nil }
func (m *mockExec) Stop(context.Context, *models.Server) error    { m.stopped++; return nil }
func (m *mockExec) Restart(context.Context, *models.Server) error { m.restarted++; return nil }
func (m *mockExec) Backup(context.Context, *models.Server) error  { m.backedup++; return nil }
func (m *mockExec) Command(_ context.Context, _ *models.Server, c string) error {
	m.commands = append(m.commands, c)
	return nil
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

func TestTickFiresDueSchedule(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Name: "wake", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &past}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	m := &mockExec{}
	New(st, m).Tick(context.Background())

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
	if !strings.HasPrefix(got.LastStatus, "ok") {
		t.Errorf("LastStatus = %q, want ok…", got.LastStatus)
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
	New(st, m).Tick(context.Background())

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
	// disabled (not returned by ListEnabledSchedules) and a future one.
	_ = st.CreateSchedule(&models.Schedule{ServerID: srv.ID, Name: "off", Cron: "* * * * *", Action: models.SchedStart, Enabled: false, NextRun: &past})
	_ = st.CreateSchedule(&models.Schedule{ServerID: srv.ID, Name: "later", Cron: "* * * * *", Action: models.SchedStart, Enabled: true, NextRun: &future})

	m := &mockExec{}
	New(st, m).Tick(context.Background())
	if m.started != 0 {
		t.Errorf("no schedule should have fired, Start called %d", m.started)
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
