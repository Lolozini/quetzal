package api_test

import (
	"bytes"
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

// TestSchedulePatchIsPartial pins PATCH semantics: a body that touches one field
// must leave the others alone. Decoding straight into the create request made an
// omitted "enabled" read as false, so a client renaming a schedule silently
// stopped it — 200, no warning, backups quietly no longer running.
func TestSchedulePatchIsPartial(t *testing.T) {
	_, admin, base := newServerForSchedules(t)
	r := post(t, admin, base, map[string]any{
		"name": "nightly", "cron": "0 3 * * *", "enabled": true,
		"tasks": []map[string]any{{"action": "backup"}},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create schedule = %d", r.StatusCode)
	}
	var sc struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&sc)
	r.Body.Close()

	patch := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPatch, base+"/"+itoa(sc.ID), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Renaming must not disable the schedule.
	code, out := patch(map[string]any{"name": "renamed"})
	if code != http.StatusOK {
		t.Fatalf("rename PATCH = %d, want 200", code)
	}
	if out["enabled"] != true {
		t.Errorf("rename silently disabled the schedule: enabled=%v", out["enabled"])
	}
	if out["cron"] != "0 3 * * *" {
		t.Errorf("rename lost the cron: %v", out["cron"])
	}
	if tasks, _ := out["tasks"].([]any); len(tasks) != 1 {
		t.Errorf("rename lost the task chain: %v", out["tasks"])
	}

	// Disabling on its own works and keeps everything else.
	code, out = patch(map[string]any{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("disable PATCH = %d, want 200", code)
	}
	if out["enabled"] != false {
		t.Errorf("enabled=%v, want false", out["enabled"])
	}
	if out["name"] != "renamed" {
		t.Errorf("disable lost the name: %v", out["name"])
	}
	if out["nextRun"] != nil {
		t.Errorf("disabled schedule kept nextRun=%v", out["nextRun"])
	}

	// And re-enabling restores a next fire time.
	if code, out = patch(map[string]any{"enabled": true}); code != http.StatusOK || out["nextRun"] == nil {
		t.Errorf("re-enable = %d, nextRun=%v", code, out["nextRun"])
	}
}

// A schedule's cron was read in the control plane's zone — UTC in a container —
// so "0 4 * * *" fired at 4am UTC, which is 6am in Paris in summer, with nothing
// saying so. The zone is now part of the schedule.
func TestScheduleCarriesATimeZone(t *testing.T) {
	_, admin, base := newServerForSchedules(t)

	r := post(t, admin, base, map[string]any{
		"name": "nightly", "cron": "0 4 * * *", "enabled": true, "timezone": "Europe/Paris",
		"tasks": []map[string]any{{"action": "restart"}},
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	var sc models.Schedule
	json.NewDecoder(r.Body).Decode(&sc)
	if sc.Timezone != "Europe/Paris" {
		t.Errorf("timezone = %q, want Europe/Paris", sc.Timezone)
	}
	if sc.NextRun == nil {
		t.Fatal("an enabled schedule should have a next run")
	}
	// 04:00 in Paris is 02:00 or 03:00 UTC depending on the season — never 04:00.
	if h := sc.NextRun.UTC().Hour(); h == 4 {
		t.Errorf("next run is %s: the zone was ignored", sc.NextRun.UTC())
	}

	// A zone that does not exist is refused rather than silently falling back.
	if rr := post(t, admin, base, map[string]any{
		"name": "bad zone", "cron": "0 4 * * *", "enabled": true, "timezone": "Mars/Olympus",
		"tasks": []map[string]any{{"action": "restart"}},
	}); rr.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown zone = %d, want 400", rr.StatusCode)
	}

	// It survives a partial PATCH that does not mention it, and can be changed.
	if rr := doPatch(t, admin, base+"/"+itoa(sc.ID), map[string]any{"name": "renamed"}); rr.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d", rr.StatusCode)
	}
	var after models.Schedule
	rr := doPatch(t, admin, base+"/"+itoa(sc.ID), map[string]any{"timezone": "Asia/Tokyo"})
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("change zone = %d", rr.StatusCode)
	}
	json.NewDecoder(rr.Body).Decode(&after)
	if after.Timezone != "Asia/Tokyo" || after.Name != "renamed" {
		t.Errorf("after patches: zone=%q name=%q", after.Timezone, after.Name)
	}
}
