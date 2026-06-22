package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/totp"
)

func freshClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	return &http.Client{Jar: jar}, err
}

func code(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return c
}

func TestTwoFactorEnrollmentAndLogin(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Enroll: setup returns a secret, enable confirms with a code.
	r := post(t, admin, ts.URL+"/api/me/2fa/setup", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("setup = %d", r.StatusCode)
	}
	var setup struct{ Secret, URI string }
	json.NewDecoder(r.Body).Decode(&setup)
	r.Body.Close()
	if setup.Secret == "" || setup.URI == "" {
		t.Fatal("setup must return a secret and otpauth URI")
	}

	// A wrong code is rejected; the right code enables 2FA and returns recovery codes.
	if r := post(t, admin, ts.URL+"/api/me/2fa/enable", map[string]string{"code": "000000"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("enable with bad code = %d, want 400", r.StatusCode)
	}
	r = post(t, admin, ts.URL+"/api/me/2fa/enable", map[string]string{"code": code(t, setup.Secret)})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("enable = %d", r.StatusCode)
	}
	var enabled struct{ RecoveryCodes []string }
	json.NewDecoder(r.Body).Decode(&enabled)
	r.Body.Close()
	if len(enabled.RecoveryCodes) != 10 {
		t.Fatalf("want 10 recovery codes, got %d", len(enabled.RecoveryCodes))
	}

	// /api/me reflects the enabled state.
	var me struct {
		ID               uint
		TwoFactorEnabled bool
	}
	getJSON(t, admin, ts.URL+"/api/me", &me)
	if !me.TwoFactorEnabled {
		t.Error("/api/me should report twoFactorEnabled=true")
	}

	// A fresh client: password alone yields a challenge, not a session.
	fresh, _ := freshClient()
	r = post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("login step 1 = %d", r.StatusCode)
	}
	var chal map[string]bool
	json.NewDecoder(r.Body).Decode(&chal)
	r.Body.Close()
	if !chal["twoFactorRequired"] {
		t.Fatal("login without code should return twoFactorRequired")
	}
	if r, _ := fresh.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session should exist after step 1, /me = %d", r.StatusCode)
	}

	// Wrong code -> 401; correct TOTP -> session.
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret", "code": "000000"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with bad code = %d, want 401", r.StatusCode)
	}
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret", "code": code(t, setup.Secret)}); r.StatusCode != http.StatusOK {
		t.Fatalf("login with TOTP = %d", r.StatusCode)
	}
	if r, _ := fresh.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("session should exist after TOTP login, /me = %d", r.StatusCode)
	}

	// A recovery code logs in once and is then consumed.
	rc, _ := freshClient()
	if r := post(t, rc, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret", "code": enabled.RecoveryCodes[0]}); r.StatusCode != http.StatusOK {
		t.Fatalf("recovery-code login = %d", r.StatusCode)
	}
	rc2, _ := freshClient()
	if r := post(t, rc2, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret", "code": enabled.RecoveryCodes[0]}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("reusing a recovery code = %d, want 401", r.StatusCode)
	}
}

func TestTwoFactorAdminReset(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Admin enrolls.
	r := post(t, admin, ts.URL+"/api/me/2fa/setup", nil)
	var setup struct{ Secret string }
	json.NewDecoder(r.Body).Decode(&setup)
	r.Body.Close()
	post(t, admin, ts.URL+"/api/me/2fa/enable", map[string]string{"code": code(t, setup.Secret)})

	var me struct{ ID uint }
	getJSON(t, admin, ts.URL+"/api/me", &me)

	// Non-admin cannot reset others. Create bob, who must not reach the endpoint.
	createUser(t, admin, ts.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	bob := loginAs(t, ts.URL, "bob", "bobpw1234")
	if r := post(t, bob, ts.URL+"/api/users/"+itoa(me.ID)+"/2fa/disable", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin reset = %d, want 403", r.StatusCode)
	}

	// Admin reset clears 2FA; password-only login works again.
	if r := post(t, admin, ts.URL+"/api/users/"+itoa(me.ID)+"/2fa/disable", nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("admin reset = %d", r.StatusCode)
	}
	fresh, _ := freshClient()
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "admin", "password": "supersecret"}); r.StatusCode != http.StatusOK {
		t.Fatalf("post-reset login = %d", r.StatusCode)
	}
	if r, _ := fresh.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Errorf("password-only login should now create a session, /me = %d", r.StatusCode)
	}
}
