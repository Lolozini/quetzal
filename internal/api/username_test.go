package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Next to the superadmin Lolozini, the recette of 0.10.0 made lolozini,
// LOLOZINI, "qa user space", "qa-<b>bold</b>", a 204-character name and one
// with a bell and a newline in it (R-12). A username is now ASCII letters,
// digits, dots, dashes and underscores, 3 to 64 of them, and a name another
// account has, case aside, is taken -- and finds that account at login.
func TestUsernamesAreToldApartAndKeptReadable(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "Lolozini", "password": "supersecret"})

	create := func(name string) int {
		t.Helper()
		r := post(t, admin, srv.URL+"/api/users", map[string]any{"username": name, "password": "longenough1", "maxServers": -1})
		r.Body.Close()
		return r.StatusCode
	}
	for _, name := range []string{"lolozini", "LOLOZINI"} {
		if got := create(name); got != http.StatusConflict {
			t.Errorf("%q next to Lolozini = %d, want 409", name, got)
		}
	}
	for _, name := range []string{"qa user space", "qa-<b>bold</b>", strings.Repeat("a", 65), "qa\abell\nnewline", "-dash-first", "ab", "Léo"} {
		if got := create(name); got != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", name, got)
		}
	}
	for _, name := range []string{"qa-user_1", "Alice.B", strings.Repeat("a", 64)} {
		if got := create(name); got != http.StatusCreated {
			t.Errorf("%q = %d, want 201", name, got)
		}
	}
	// Login finds the account whatever the case typed.
	if c := loginAs(t, srv.URL, "alice.b", "longenough1"); c == nil {
		t.Error("login with another case failed")
	}
}
