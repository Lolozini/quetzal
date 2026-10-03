package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

// A restart deleted the game's pod: the game got SIGTERM alone, and the next
// pod could start on the volume while the old one was still saving to it. It
// is left to the controller now, which stops the game with its stop command and
// starts it once the pod is gone.
func TestRestartIsAStopThenAStart(t *testing.T) {
	srv, admin, st := newTestServerStore(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process"})
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("create server: %v", err)
	}
	r.Body.Close()
	power := func(action string) {
		t.Helper()
		r := post(t, admin, srv.URL+"/api/servers/"+itoa(created.ID)+"/power", map[string]string{"action": action})
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", action, r.StatusCode)
		}
	}

	// Stopped: nothing to restart.
	power("restart")
	if got, _ := st.GetServer(created.ID); got.RestartRequestedAt != nil || got.DesiredState != models.StateStopped {
		t.Fatalf("a stopped server was restarted: %+v", got)
	}
	power("start")
	power("restart")
	got, _ := st.GetServer(created.ID)
	if got.RestartRequestedAt == nil {
		t.Fatal("the restart was not handed to the controller")
	}
	if got.Replicas() != 0 {
		t.Error("the server is asked to keep running during the stopping half of its restart")
	}
}
