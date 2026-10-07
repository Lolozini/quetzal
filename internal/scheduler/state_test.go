package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

func TestDelayedChainHonorsCancellation(t *testing.T) {
	for _, change := range []string{"disable", "delete", "disable-enable"} {
		t.Run(change, func(t *testing.T) {
			st := testStore(t)
			srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
			if err := st.CreateServer(srv); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Minute)
			sc := &models.Schedule{ServerID: srv.ID, Cron: "* * * * *", Enabled: true, NextRun: &past,
				Tasks: []models.ScheduleTask{{Action: models.SchedCommand, Payload: "warning"}, {Action: models.SchedStop, TimeOffset: 60}}}
			if err := st.CreateSchedule(sc); err != nil {
				t.Fatal(err)
			}
			m := &mockExec{st: st}
			s := New(st, m)
			s.Sleep = func(context.Context, time.Duration) error {
				if change == "delete" {
					return st.DeleteSchedule(sc.ID)
				}
				current, err := st.GetSchedule(sc.ID)
				if err != nil {
					return err
				}
				current.Enabled, current.NextRun = false, nil
				if err := st.UpdateSchedule(current); err != nil {
					return err
				}
				if change == "disable-enable" {
					current.Enabled = true
					future := time.Now().Add(time.Hour)
					current.NextRun = &future
					return st.UpdateSchedule(current)
				}
				return nil
			}
			runTick(s, context.Background())
			if got := strings.Join(m.sequence(), ","); got != "command" {
				t.Errorf("after cancellation executed %q, want only the warning", got)
			}
			current, err := st.GetSchedule(sc.ID)
			if change == "delete" {
				if err != store.ErrNotFound {
					t.Errorf("deleted schedule returned %+v, %v", current, err)
				}
			} else if err != nil || current.Run != nil {
				t.Errorf("cancelled checkpoint survived: %+v, %v", current, err)
			}
		})
	}
}

func TestInterruptedChainRejectsTaskEditAndResumesOriginal(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedStop}, {Action: models.SchedBackup, TimeOffset: 60}, {Action: models.SchedStart}}}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &mockExec{st: st}
	s := New(st, m)
	s.Sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	runTick(s, ctx)
	current, err := st.GetSchedule(sc.ID)
	if err != nil || current.Run == nil || current.Run.Next != 1 {
		t.Fatalf("missing interrupted checkpoint: %+v, %v", current, err)
	}
	current.Name = "renamed while interrupted"
	if err := st.UpdateSchedule(current); err != nil {
		t.Fatalf("rename must remain allowed: %v", err)
	}
	current.Tasks = []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedStart}}
	if err := st.UpdateSchedule(current); err == nil {
		t.Error("task edit accepted while an interrupted chain is active")
	}
	next := New(st, m)
	next.Sleep = func(context.Context, time.Duration) error { return nil }
	runTick(next, context.Background())
	if got := strings.Join(m.sequence(), ","); got != "stop,backup,start" {
		t.Errorf("resumed chain executed %q, want original stop,backup,start", got)
	}
}

type cancelBackupExec struct {
	mockExec
	afterCreate func() error
}

func (m *cancelBackupExec) Backup(ctx context.Context, srv *models.Server) (uint, error) {
	id, err := m.mockExec.Backup(ctx, srv)
	if err == nil && m.afterCreate != nil {
		err = m.afterCreate()
	}
	return id, err
}

func TestCancelledBackupCannotResurrectCheckpoint(t *testing.T) {
	for _, at := range []string{"request", "poll"} {
		t.Run(at, func(t *testing.T) {
			st := testStore(t)
			srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
			if err := st.CreateServer(srv); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Minute)
			sc := &models.Schedule{ServerID: srv.ID, Cron: "* * * * *", Enabled: true, NextRun: &past,
				Tasks: []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedStart}}}
			if err := st.CreateSchedule(sc); err != nil {
				t.Fatal(err)
			}
			disable := func() error {
				current, err := st.GetSchedule(sc.ID)
				if err != nil {
					return err
				}
				current.Enabled = false
				return st.UpdateSchedule(current)
			}
			m := &cancelBackupExec{mockExec: mockExec{st: st, pending: true}}
			if at == "request" {
				m.afterCreate = disable
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := New(st, m)
			s.Sleep = func(context.Context, time.Duration) error {
				if at == "poll" {
					if err := disable(); err != nil {
						return err
					}
				}
				cancel()
				return ctx.Err()
			}
			runTick(s, ctx)
			current, err := st.GetSchedule(sc.ID)
			if err != nil || current.Enabled || current.Run != nil {
				t.Errorf("cancelled backup checkpoint resurrected: %+v, %v", current, err)
			}
			if len(m.backups) != 1 {
				t.Fatalf("created backups = %v, want one", m.backups)
			}
			b, err := st.GetBackup(m.backups[0])
			if err != nil || b.Phase != models.BackupPending {
				t.Errorf("cancellation lost the independent backup: %+v, %v", b, err)
			}
			if m.started != 0 {
				t.Errorf("cancelled chain started the server %d times", m.started)
			}
		})
	}
}

type blockedCommandExec struct {
	mockExec
	entered chan struct{}
	release chan struct{}
}

func (m *blockedCommandExec) Command(ctx context.Context, srv *models.Server, command string) error {
	close(m.entered)
	<-m.release
	return m.mockExec.Command(ctx, srv, command)
}

func TestCancelledRunCannotFinishReplacement(t *testing.T) {
	st := testStore(t)
	srv := &models.Server{Slug: "s", Namespace: "ns", DesiredState: models.StateRunning}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	past := now.Add(-time.Minute)
	sc := &models.Schedule{ServerID: srv.ID, Cron: "* * * * *", Enabled: true, NextRun: &past,
		Tasks: []models.ScheduleTask{{Action: models.SchedCommand, Payload: "already dispatched", TimeOffset: 60}}}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	ex := &blockedCommandExec{mockExec: mockExec{st: st}, entered: make(chan struct{}), release: make(chan struct{})}
	old := New(st, ex)
	old.Now = func() time.Time { return now }
	old.Sleep = func(context.Context, time.Duration) error { return nil }
	old.Tick(context.Background())
	<-ex.entered
	current, err := st.GetSchedule(sc.ID)
	if err != nil {
		close(ex.release)
		old.Wait()
		t.Fatal(err)
	}
	current.Enabled = false
	if err := st.UpdateSchedule(current); err != nil {
		close(ex.release)
		old.Wait()
		t.Fatal(err)
	}
	current.Enabled, current.NextRun = true, &past
	current.Tasks = []models.ScheduleTask{{Action: models.SchedStart, TimeOffset: 60}}
	if err := st.UpdateSchedule(current); err != nil {
		close(ex.release)
		old.Wait()
		t.Fatal(err)
	}
	ex2 := &mockExec{st: st}
	next := New(st, ex2)
	next.Now = func() time.Time { return now.Add(10 * time.Second) }
	waiting, release := make(chan struct{}), make(chan struct{})
	next.Sleep = func(context.Context, time.Duration) error {
		close(waiting)
		<-release
		return nil
	}
	next.Tick(context.Background())
	<-waiting
	close(ex.release)
	old.Wait()
	replacement, err := st.GetSchedule(sc.ID)
	if err != nil || replacement.Run == nil || !replacement.Run.Fired.Equal(now.Add(10*time.Second)) {
		t.Errorf("obsolete completion erased the replacement run: %+v, %v", replacement, err)
	}
	close(release)
	next.Wait()
	if ex2.started != 1 {
		t.Errorf("replacement started %d times, want once", ex2.started)
	}
}
