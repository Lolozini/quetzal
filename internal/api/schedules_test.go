package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// newServerForSchedules sets up an admin, creates a server, and returns its
// schedules base URL.
func newServerForSchedules(t *testing.T) (string, *http.Client, string) {
	t.Helper()
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "sched", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	return srv.URL, admin, srv.URL + "/api/servers/" + itoa(created.ID) + "/schedules"
}

func TestScheduleChainCreate(t *testing.T) {
	_, admin, base := newServerForSchedules(t)

	r := post(t, admin, base, map[string]any{
		"name": "graceful restart", "cron": "0 5 * * *", "enabled": true,
		"tasks": []map[string]any{
			{"action": "command", "payload": "say restarting"},
			{"action": "stop", "timeOffset": 30},
			{"action": "backup", "timeOffset": 10},
			{"action": "start"},
		},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create chain = %d", r.StatusCode)
	}
	var sc models.Schedule
	json.NewDecoder(r.Body).Decode(&sc)
	if len(sc.Tasks) != 4 {
		t.Fatalf("tasks = %d, want 4", len(sc.Tasks))
	}
	if sc.Tasks[1].Action != models.SchedStop || sc.Tasks[1].TimeOffset != 30 {
		t.Errorf("task 2 = %+v, want stop@30s", sc.Tasks[1])
	}
	// The legacy Action mirrors the first task for old clients.
	if sc.Action != models.SchedCommand {
		t.Errorf("mirrored action = %q, want command", sc.Action)
	}
	if sc.NextRun == nil {
		t.Error("enabled schedule should have a NextRun")
	}
}

func TestScheduleLegacySingleActionStillWorks(t *testing.T) {
	_, admin, base := newServerForSchedules(t)
	// Old clients send a bare action/payload with no tasks.
	r := post(t, admin, base, map[string]any{
		"name": "nightly", "cron": "0 4 * * *", "action": "restart", "enabled": true,
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("legacy create = %d", r.StatusCode)
	}
	var sc models.Schedule
	json.NewDecoder(r.Body).Decode(&sc)
	if len(sc.Tasks) != 1 || sc.Tasks[0].Action != models.SchedRestart {
		t.Errorf("legacy action not normalized into a 1-task chain: %+v", sc.Tasks)
	}
}

func TestScheduleChainValidation(t *testing.T) {
	_, admin, base := newServerForSchedules(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"command without payload", map[string]any{"name": "a", "cron": "* * * * *",
			"tasks": []map[string]any{{"action": "command"}}}},
		{"unknown action", map[string]any{"name": "a", "cron": "* * * * *",
			"tasks": []map[string]any{{"action": "explode"}}}},
		{"negative offset", map[string]any{"name": "a", "cron": "* * * * *",
			"tasks": []map[string]any{{"action": "stop", "timeOffset": -1}}}},
		{"offset too large", map[string]any{"name": "a", "cron": "* * * * *",
			"tasks": []map[string]any{{"action": "stop", "timeOffset": 999999}}}},
		{"no tasks at all", map[string]any{"name": "a", "cron": "* * * * *"}},
		{"bad cron", map[string]any{"name": "a", "cron": "nope",
			"tasks": []map[string]any{{"action": "stop"}}}},
	}
	for _, c := range cases {
		r := post(t, admin, base, c.body)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.name, r.StatusCode)
		}
	}
}

func TestScheduleUpdateToChain(t *testing.T) {
	_, admin, base := newServerForSchedules(t)
	r := post(t, admin, base, map[string]any{"name": "x", "cron": "0 4 * * *", "action": "restart", "enabled": true})
	var sc models.Schedule
	json.NewDecoder(r.Body).Decode(&sc)

	pr := doPatch(t, admin, base+"/"+itoa(sc.ID), map[string]any{
		"name": "x", "cron": "0 4 * * *", "enabled": true,
		"tasks": []map[string]any{
			{"action": "stop"},
			{"action": "start", "timeOffset": 5},
		},
	})
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", pr.StatusCode)
	}
	var updated models.Schedule
	json.NewDecoder(pr.Body).Decode(&updated)
	if len(updated.Tasks) != 2 || updated.Tasks[1].TimeOffset != 5 {
		t.Errorf("updated tasks = %+v, want 2 with start@5s", updated.Tasks)
	}
}
