package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

func TestActiveScheduleEditsPreserveCheckpoint(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	response := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "sched", "template": "generic-process"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", response.StatusCode)
	}
	var srv models.Server
	if err := json.NewDecoder(response.Body).Decode(&srv); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sc := &models.Schedule{ServerID: srv.ID, Name: "nightly", Cron: "0 4 * * *", Enabled: true,
		Tasks: []models.ScheduleTask{{Action: models.SchedBackup}, {Action: models.SchedStart}},
		Run:   &models.ScheduleRun{Fired: now, Due: now, Backup: 42}}
	if err := st.CreateSchedule(sc); err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/servers/" + itoa(srv.ID) + "/schedules/" + itoa(sc.ID)
	for _, body := range []map[string]any{
		{"tasks": []map[string]any{{"action": "start"}}},
		{"action": "restart"},
	} {
		r := doPatch(t, admin, url, body)
		r.Body.Close()
		if r.StatusCode != http.StatusConflict {
			t.Errorf("task edit on active schedule = %d, want 409", r.StatusCode)
		}
	}
	r := doPatch(t, admin, url, map[string]any{"name": "renamed"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("rename active schedule = %d, want 200", r.StatusCode)
	}
	got, err := st.GetSchedule(sc.ID)
	if err != nil || got.Name != "renamed" || got.Run == nil || got.Run.Backup != 42 || len(got.TaskChain()) != 2 {
		t.Fatalf("rename lost original chain/checkpoint: %+v, %v", got, err)
	}
	r = doPatch(t, admin, url, map[string]any{"enabled": false})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("disable active schedule = %d, want 200", r.StatusCode)
	}
	got, err = st.GetSchedule(sc.ID)
	if err != nil || got.Enabled || got.Run != nil {
		t.Fatalf("disable did not cancel chain: %+v, %v", got, err)
	}
	r = doPatch(t, admin, url, map[string]any{"tasks": []map[string]any{{"action": "start"}}})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("task edit after cancellation = %d, want 200", r.StatusCode)
	}
}
