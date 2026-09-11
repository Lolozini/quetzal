package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A subuser holding "schedules" must not reach, through a scheduled task, an
// action they cannot perform by hand. Before this check, "schedules" alone let
// them write to the console (op themselves on a Minecraft server), cycle the
// power and take backups.
func TestScheduleTasksNeedTheMatchingPermission(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, srv.URL, map[string]any{"username": "mallory", "password": "mallorypw1"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")
	mallory := loginAs(t, srv.URL, "mallory", "mallorypw1")

	var created struct{ ID uint }
	r := post(t, alice, srv.URL+"/api/servers", map[string]any{"name": "victim", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := srv.URL + "/api/servers/" + itoa(created.ID)

	if rr := post(t, alice, url+"/access", map[string]any{
		"username": "mallory", "permissions": []string{"view", "schedules"},
	}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}

	for _, tc := range []struct {
		name string
		task map[string]any
	}{
		{"console", map[string]any{"action": "command", "payload": "op mallory"}},
		{"power", map[string]any{"action": "start"}},
		{"backups", map[string]any{"action": "backup"}},
	} {
		rr := post(t, mallory, url+"/schedules", map[string]any{
			"name": tc.name, "cron": "* * * * *", "enabled": true,
			"tasks": []map[string]any{tc.task},
		})
		if rr.StatusCode != http.StatusForbidden {
			t.Errorf("%s task without the permission = %d, want 403", tc.name, rr.StatusCode)
		}
	}

	// A chain is refused whole: one unauthorized task poisons it, even behind a
	// task the subuser may run.
	if rr := post(t, mallory, url+"/schedules", map[string]any{
		"name": "mixed", "cron": "* * * * *", "enabled": true,
		"tasks": []map[string]any{
			{"action": "backup"},
			{"action": "command", "payload": "op mallory"},
		},
	}); rr.StatusCode != http.StatusForbidden {
		t.Errorf("mixed chain = %d, want 403", rr.StatusCode)
	}

	// With the permission granted, the same task goes through.
	if rr := post(t, alice, url+"/access", map[string]any{
		"username": "mallory", "permissions": []string{"view", "schedules", "console"},
	}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("regrant = %d", rr.StatusCode)
	}
	var sc struct{ ID uint }
	rr := post(t, mallory, url+"/schedules", map[string]any{
		"name": "allowed", "cron": "* * * * *", "enabled": true,
		"tasks": []map[string]any{{"action": "command", "payload": "say hello"}},
	})
	if rr.StatusCode != http.StatusCreated {
		t.Fatalf("command task with console permission = %d, want 201", rr.StatusCode)
	}
	json.NewDecoder(rr.Body).Decode(&sc)

	// A PATCH must not be a way around the check either.
	if pr := doPatch(t, mallory, url+"/schedules/"+itoa(sc.ID), map[string]any{
		"tasks": []map[string]any{{"action": "start"}},
	}); pr.StatusCode != http.StatusForbidden {
		t.Errorf("PATCH to a power task without the permission = %d, want 403", pr.StatusCode)
	}
	// The legacy single-action form is validated the same way.
	if pr := doPatch(t, mallory, url+"/schedules/"+itoa(sc.ID), map[string]any{
		"action": "restart",
	}); pr.StatusCode != http.StatusForbidden {
		t.Errorf("PATCH legacy action = %d, want 403", pr.StatusCode)
	}

	// De-escalation stays open: a subuser who can manage schedules may switch off
	// a task the owner wrote, even one they could never have written themselves —
	// a disabled schedule runs nothing. Arming it again is another matter.
	or := post(t, alice, url+"/schedules", map[string]any{
		"name": "owner cron", "cron": "0 5 * * *", "enabled": true,
		"tasks": []map[string]any{{"action": "backup"}},
	})
	if or.StatusCode != http.StatusCreated {
		t.Fatalf("owner schedule = %d", or.StatusCode)
	}
	var owned struct{ ID uint }
	json.NewDecoder(or.Body).Decode(&owned)
	// mallory holds view+schedules+console at this point, but not backups.
	if pr := doPatch(t, mallory, url+"/schedules/"+itoa(owned.ID), map[string]any{
		"enabled": false,
	}); pr.StatusCode != http.StatusOK {
		t.Errorf("disabling a backup schedule = %d, want 200", pr.StatusCode)
	}
	if pr := doPatch(t, mallory, url+"/schedules/"+itoa(owned.ID), map[string]any{
		"enabled": true,
	}); pr.StatusCode != http.StatusForbidden {
		t.Errorf("re-enabling it = %d, want 403", pr.StatusCode)
	}
	// Nor may they rewrite a disabled chain into something they cannot run: that
	// would only wait for the owner to switch it back on.
	if pr := doPatch(t, mallory, url+"/schedules/"+itoa(owned.ID), map[string]any{
		"tasks": []map[string]any{{"action": "start"}},
	}); pr.StatusCode != http.StatusForbidden {
		t.Errorf("rewriting a disabled chain = %d, want 403", pr.StatusCode)
	}

	// An action the permission map does not know is refused rather than waved
	// through — the one that matters is the action someone adds later.
	if rr := post(t, alice, url+"/schedules", map[string]any{
		"name": "unknown", "cron": "* * * * *", "enabled": true,
		"tasks": []map[string]any{{"action": "rm-rf"}},
	}); rr.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown action = %d, want 400", rr.StatusCode)
	}

	// The owner keeps full run of their own server.
	if rr := post(t, alice, url+"/schedules", map[string]any{
		"name": "owner", "cron": "* * * * *", "enabled": true,
		"tasks": []map[string]any{{"action": "command", "payload": "say hi"}, {"action": "backup"}},
	}); rr.StatusCode != http.StatusCreated {
		t.Errorf("owner chain = %d, want 201", rr.StatusCode)
	}
}
