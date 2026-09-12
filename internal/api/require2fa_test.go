package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func setRequire2FA(t *testing.T, admin *http.Client, base, mode string) int {
	t.Helper()
	return doPut(t, admin, base+"/api/security-settings", map[string]any{"requireTwoFactor": mode}).StatusCode
}

// Requiring a second factor has to be enforceable without locking everyone out
// the moment it is turned on: a session stays valid, but reaches only enrolment
// until the account has one.
func TestRequireTwoFactorGatesUntilEnrolled(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")

	// Off by default: nothing changes.
	if r, _ := alice.Get(ts.URL + "/api/servers"); r.StatusCode != http.StatusOK {
		t.Fatalf("precondition: servers = %d", r.StatusCode)
	}

	if code := setRequire2FA(t, admin, ts.URL, "all"); code != http.StatusOK {
		t.Fatalf("set policy = %d", code)
	}

	// The existing session survives — otherwise turning this on strands every
	// account at once, the superadmin included.
	r, _ := alice.Get(ts.URL + "/api/me")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("/api/me under the requirement = %d, want 200", r.StatusCode)
	}
	var me struct {
		Username          string `json:"username"`
		TwoFactorRequired bool   `json:"twoFactorRequired"`
	}
	json.NewDecoder(r.Body).Decode(&me)
	r.Body.Close()
	if !me.TwoFactorRequired {
		t.Error("/api/me should say a second factor is owed")
	}

	// But nothing else is reachable.
	for _, path := range []string{"/api/servers", "/api/templates", "/api/me/sshkeys"} {
		rr, _ := alice.Get(ts.URL + path)
		if rr.StatusCode != http.StatusForbidden {
			t.Errorf("%s = %d, want 403 while a second factor is owed", path, rr.StatusCode)
		}
	}
	// Enrolment is, or the requirement could never be satisfied.
	if rr := post(t, alice, ts.URL+"/api/me/2fa/setup", nil); rr.StatusCode != http.StatusOK {
		t.Fatalf("2fa setup = %d, want 200", rr.StatusCode)
	}

	// Logging in fresh still works: the wall is on what a session reaches, not on
	// the credential check.
	if _, err := freshClient(); err != nil {
		t.Fatal(err)
	}
	second := loginAs(t, ts.URL, "alice", "alicepw12")
	if rr, _ := second.Get(ts.URL + "/api/servers"); rr.StatusCode != http.StatusForbidden {
		t.Errorf("a fresh login reaches %d, want 403", rr.StatusCode)
	}
}

// "admins" leaves ordinary accounts alone.
func TestRequireTwoFactorForAdminsOnly(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	roleID := createRole(t, admin, ts.URL, "ops", []string{"servers"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mel", "password": "melpw12345"})
	if rr := setUserRole(t, admin, ts.URL, userID(t, admin, ts.URL, "mel"), &roleID); rr.StatusCode != http.StatusOK {
		t.Fatalf("assign role = %d", rr.StatusCode)
	}
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	mel := loginAs(t, ts.URL, "mel", "melpw12345")

	if code := setRequire2FA(t, admin, ts.URL, "admins"); code != http.StatusOK {
		t.Fatalf("set policy = %d", code)
	}
	if rr, _ := alice.Get(ts.URL + "/api/servers"); rr.StatusCode != http.StatusOK {
		t.Errorf("a regular account = %d, want 200 under \"admins\"", rr.StatusCode)
	}
	// A scoped admin counts as an admin.
	if rr, _ := mel.Get(ts.URL + "/api/servers"); rr.StatusCode != http.StatusForbidden {
		t.Errorf("a scoped admin = %d, want 403", rr.StatusCode)
	}
	// And so does the superadmin, who is not exempt from their own policy.
	if rr, _ := admin.Get(ts.URL + "/api/servers"); rr.StatusCode != http.StatusForbidden {
		t.Errorf("the superadmin = %d, want 403", rr.StatusCode)
	}
}

// The policy decides who gets in, so only a superadmin sets it.
func TestRequireTwoFactorIsSuperadminOnly(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	roleID := createRole(t, admin, ts.URL, "settings-admin", []string{"settings"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mel", "password": "melpw12345"})
	if rr := setUserRole(t, admin, ts.URL, userID(t, admin, ts.URL, "mel"), &roleID); rr.StatusCode != http.StatusOK {
		t.Fatalf("assign role = %d", rr.StatusCode)
	}
	mel := loginAs(t, ts.URL, "mel", "melpw12345")

	// A settings-admin may read the policy but not change it.
	if rr, _ := mel.Get(ts.URL + "/api/security-settings"); rr.StatusCode != http.StatusOK {
		t.Errorf("read = %d, want 200", rr.StatusCode)
	}
	if code := setRequire2FA(t, mel, ts.URL, "all"); code != http.StatusForbidden {
		t.Errorf("a scoped settings-admin set the policy: %d", code)
	}
	// A nonsense value is refused rather than stored.
	if code := setRequire2FA(t, admin, ts.URL, "sometimes"); code != http.StatusBadRequest {
		t.Errorf("bad value = %d, want 400", code)
	}
}
