package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
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

	// Whoever requires a second factor holds one (TestRequiringTwoFactorNeedsOneFirst).
	enrollTOTP(t, ts.URL, admin, uint64(time.Now().Unix())/totpStep)
	if code := setRequire2FA(t, admin, ts.URL, "all"); code != http.StatusOK {
		t.Fatalf("set policy = %d", code)
	}

	// The existing session survives — otherwise turning this on strands every
	// account at once.
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

	enrollTOTP(t, ts.URL, admin, uint64(time.Now().Unix())/totpStep)
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
}

// A superadmin without a second factor who required one, of everyone or of
// administrators, was covered by their own policy the moment it was saved: the
// enrolment page and nothing else, their API keys refused, turning it back off
// included, and nothing said so beforehand. Whoever requires a second factor
// now holds one first.
func TestRequiringTwoFactorNeedsOneFirst(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var key struct{ Token string }
	r := post(t, admin, ts.URL+"/api/apikeys", map[string]string{"name": "automation"})
	json.NewDecoder(r.Body).Decode(&key)
	r.Body.Close()
	withKey := func() int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/servers", nil)
		req.Header.Set("Authorization", "Bearer "+key.Token)
		rr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rr.Body.Close()
		return rr.StatusCode
	}

	for _, mode := range []string{"all", "admins"} {
		if code := setRequire2FA(t, admin, ts.URL, mode); code != http.StatusConflict {
			t.Errorf("%q without a second factor of one's own = %d, want 409", mode, code)
		}
	}
	if code := withKey(); code != http.StatusOK {
		t.Errorf("the superadmin's API key = %d, want 200: the policy went through", code)
	}
	if code := setRequire2FA(t, admin, ts.URL, "off"); code != http.StatusOK {
		t.Errorf("off = %d, want 200", code)
	}

	enrollTOTP(t, ts.URL, admin, uint64(time.Now().Unix())/totpStep)
	if code := setRequire2FA(t, admin, ts.URL, "all"); code != http.StatusOK {
		t.Errorf("with a second factor = %d, want 200", code)
	}
	if code := withKey(); code != http.StatusOK {
		t.Errorf("the superadmin's API key under their own policy = %d, want 200", code)
	}
}

// What a policy will hold back is shown before it is turned on: the accounts it
// covers that have no second factor, and the API keys they hold, which stop
// working until those accounts enrol.
func TestTwoFactorPolicyShowsWhoItHoldsBack(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	enrollTOTP(t, ts.URL, admin, uint64(time.Now().Unix())/totpStep)
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	createUser(t, admin, ts.URL, map[string]any{"username": "mel", "password": "melpw12345"})
	roleID := createRole(t, admin, ts.URL, "ops", []string{"servers"})
	if rr := setUserRole(t, admin, ts.URL, userID(t, admin, ts.URL, "mel"), &roleID); rr.StatusCode != http.StatusOK {
		t.Fatalf("assign role = %d", rr.StatusCode)
	}
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	mel := loginAs(t, ts.URL, "mel", "melpw12345")
	for c, n := range map[*http.Client]int{alice: 2, mel: 1, admin: 1} {
		for i := 0; i < n; i++ {
			if rr := post(t, c, ts.URL+"/api/apikeys", map[string]string{"name": "k"}); rr.StatusCode != http.StatusCreated {
				t.Fatalf("create key = %d", rr.StatusCode)
			}
		}
	}

	var got struct {
		Impact map[string]struct{ Accounts, APIKeys int }
	}
	getJSON(t, admin, ts.URL+"/api/security-settings", &got)
	want := map[string]struct{ Accounts, APIKeys int }{
		"off":    {0, 0},
		"admins": {1, 1}, // mel; the superadmin has a second factor
		"all":    {2, 3}, // alice and mel
	}
	for mode, w := range want {
		if got.Impact[mode] != w {
			t.Errorf("impact of %q = %+v, want %+v", mode, got.Impact[mode], w)
		}
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
