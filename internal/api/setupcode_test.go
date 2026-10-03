package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Until someone had made the first account, anyone reaching the panel could
// make themselves its superadmin, and the install guide publishes it at once
// (recette of 0.10.0, R-25). The setup asks for a code the panel prints in its
// log, and forgets it once the account exists.
func TestTheFirstAccountNeedsTheSetupCode(t *testing.T) {
	srv, c, st, apiSrv, _ := newTestServerFull(t)
	apiSrv.RequireSetupCode = true

	var status struct{ Needed, CodeRequired bool }
	getJSON(t, c, srv.URL+"/api/setup/status", &status)
	if !status.Needed || !status.CodeRequired {
		t.Fatalf("status %+v, want the setup needed and its code asked for", status)
	}
	code, err := st.SetupCode()
	if err != nil || len(code) != 14 {
		t.Fatalf("setup code %q (%v)", code, err)
	}
	if again, _ := st.SetupCode(); again != code {
		t.Errorf("the code changed from %q to %q: a restart would print another", code, again)
	}
	for _, typed := range []string{"", "AAAA-BBBB-CCCC"} {
		r := post(t, c, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret", "setupCode": typed})
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("setup with code %q = %d, want 403", typed, r.StatusCode)
		}
	}
	// Typed in lower case and without its dashes, it is the same code.
	typed := ""
	for _, ch := range code {
		if ch != '-' {
			typed += string(ch | 0x20)
		}
	}
	r := post(t, c, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret", "setupCode": typed})
	var u struct{ Username string }
	json.NewDecoder(r.Body).Decode(&u)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated || u.Username != "admin" {
		t.Fatalf("setup with the code = %d", r.StatusCode)
	}
	if left, _ := st.SetupCode(); left != "" {
		t.Errorf("the code %q is still there once the account exists", left)
	}
}
