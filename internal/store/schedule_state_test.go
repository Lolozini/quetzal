package store

import (
	"errors"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func TestScheduleClaimsAndCheckpointsAreGenerationBound(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	next := now.Add(time.Hour)
	sc := &models.Schedule{ServerID: 1, Cron: "* * * * *", Enabled: true, NextRun: &now,
		Tasks: []models.ScheduleTask{{Action: models.SchedStop}, {Action: models.SchedStart}}}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	stale := *sc
	sc.Enabled = false
	if err := st.UpdateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	if started, err := st.StartScheduleRun(&stale, &models.ScheduleRun{Fired: now, Due: now}, &next); err != nil || started {
		t.Fatalf("disabled snapshot claimed a run: %v, %v", started, err)
	}
	sc.Enabled = true
	sc.Tasks = []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedStart}}
	if err := st.UpdateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	if started, err := st.StartScheduleRun(&stale, &models.ScheduleRun{Fired: now, Due: now}, &next); err != nil || started {
		t.Fatalf("obsolete tasks claimed a run: %v, %v", started, err)
	}
	run := &models.ScheduleRun{Fired: now, Due: now}
	if started, err := st.StartScheduleRun(sc, run, &next); err != nil || !started {
		t.Fatalf("current configuration not claimed: %v, %v", started, err)
	}
	if started, err := st.StartScheduleRun(sc, &models.ScheduleRun{Fired: now, Due: now}, &next); err != nil || started {
		t.Fatalf("overlapping claimant started: %v, %v", started, err)
	}
	run.Next = 1
	if saved, err := st.SaveScheduleRun(sc.ID, run); err != nil || !saved {
		t.Fatalf("live checkpoint not saved: %v, %v", saved, err)
	}
	current, err := st.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Tasks = []models.ScheduleTask{{Action: models.SchedRestart}}
	if err := st.UpdateSchedule(current); !errors.Is(err, ErrScheduleActive) {
		t.Fatalf("active task edit = %v, want ErrScheduleActive", err)
	}
	if finished, err := st.FinishScheduleRun(sc.ID, run, "original result"); err != nil || !finished {
		t.Fatalf("live finish rejected: %v, %v", finished, err)
	}
	if saved, err := st.SaveScheduleRun(sc.ID, run); err != nil || saved {
		t.Fatalf("finished checkpoint resurrected: %v, %v", saved, err)
	}
	current, err = st.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &models.ScheduleRun{Fired: next, Due: next}
	if started, err := st.StartScheduleRun(current, replacement, &next); err != nil || !started {
		t.Fatalf("replacement not claimed: %v, %v", started, err)
	}
	if saved, err := st.SaveScheduleRun(sc.ID, run); err != nil || saved {
		t.Fatalf("old checkpoint replaced new generation: %v, %v", saved, err)
	}
	if finished, err := st.FinishScheduleRun(sc.ID, run, "obsolete result"); err != nil || finished {
		t.Fatalf("old completion ended new generation: %v, %v", finished, err)
	}
	current, err = st.GetSchedule(sc.ID)
	if err != nil || current.Run == nil || current.Run.Generation != replacement.Generation || current.LastStatus != "original result" {
		t.Fatalf("replacement or result damaged: %+v, %v", current, err)
	}
	if err := st.DeleteSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	if saved, err := st.SaveScheduleRun(sc.ID, replacement); err != nil || saved {
		t.Fatalf("deleted run resurrected: %v, %v", saved, err)
	}
}
