package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/totp"
)

// totpStep is the 30-second time step TOTP counts in.
const totpStep = 30

// alignToStep waits, if needed, until a fresh step has just begun, so a test
// that reasons about step-1 / step / step+1 cannot have the boundary move under
// it mid-run. Costs nothing on most runs and at most a few seconds otherwise.
func alignToStep(t *testing.T) uint64 {
	t.Helper()
	if left := totpStep - time.Now().Unix()%totpStep; left < 10 {
		time.Sleep(time.Duration(left) * time.Second)
	}
	return uint64(time.Now().Unix()) / totpStep
}

// codeForStep returns the code for an explicit time step.
func codeForStep(t *testing.T, secret string, step uint64) string {
	t.Helper()
	c, err := totp.Code(secret, time.Unix(int64(step*totpStep), 0))
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return c
}

// enrollTOTP turns on 2FA using the code for the given step and returns the
// secret. Enrolling on an explicit step leaves the neighbouring ones free for
// the test to spend.
func enrollTOTP(t *testing.T, ts string, admin *http.Client, step uint64) string {
	t.Helper()
	r := post(t, admin, ts+"/api/me/2fa/setup", nil)
	var setup struct{ Secret string }
	json.NewDecoder(r.Body).Decode(&setup)
	r.Body.Close()
	if setup.Secret == "" {
		t.Fatal("setup returned no secret")
	}
	if rr := post(t, admin, ts+"/api/me/2fa/enable",
		map[string]string{"code": codeForStep(t, setup.Secret, step)}); rr.StatusCode != http.StatusOK {
		t.Fatalf("enable = %d", rr.StatusCode)
	}
	return setup.Secret
}

// A TOTP code is valid across a three-step window — up to 90 seconds — so
// accepting one without spending its step lets a code seen once (over a
// shoulder, in a screenshot) be used again while it is still the legitimate
// user's own code.
func TestTOTPCodeCannotBeReplayed(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	now := alignToStep(t)
	// Enroll on the previous step so the current one and the next are still free.
	secret := enrollTOTP(t, ts.URL, admin, now-1)

	login := func(client *http.Client, code string) int {
		rr := post(t, client, ts.URL+"/api/login", map[string]string{
			"username": "admin", "password": "supersecret", "code": code,
		})
		defer rr.Body.Close()
		return rr.StatusCode
	}

	c := codeForStep(t, secret, now)
	first, _ := freshClient()
	if got := login(first, c); got != http.StatusOK {
		t.Fatalf("first use of the code = %d, want 200", got)
	}
	// Same code, moments later, still well inside its window.
	second, _ := freshClient()
	if got := login(second, c); got != http.StatusUnauthorized {
		t.Errorf("replayed code = %d, want 401", got)
	}
	if rr, _ := second.Get(ts.URL + "/api/me"); rr.StatusCode == http.StatusOK {
		t.Error("the replayed login produced a usable session")
	}
	// A code from a step that has not been spent still works: the guard burns
	// one step, it does not wedge the account.
	third, _ := freshClient()
	if got := login(third, codeForStep(t, secret, now+1)); got != http.StatusOK {
		t.Errorf("a fresh code after a replay = %d, want 200", got)
	}
	// And an older step is refused even though it is inside the window: the mark
	// is a high-water mark, not a set of seen codes.
	fourth, _ := freshClient()
	if got := login(fourth, c); got != http.StatusUnauthorized {
		t.Errorf("an older step after a newer one = %d, want 401", got)
	}
}

// The enrolling code is spent too, so it cannot double as the first login.
func TestTOTPEnrollmentCodeIsSpent(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	now := alignToStep(t)
	secret := enrollTOTP(t, ts.URL, admin, now)

	other, _ := freshClient()
	rr := post(t, other, ts.URL+"/api/login", map[string]string{
		"username": "admin", "password": "supersecret", "code": codeForStep(t, secret, now),
	})
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the enrolling code = %d, want 401", rr.StatusCode)
	}
}

// Turning 2FA off and straight back on must work inside the same 30-second
// window. The spent-step mark belongs to the old secret, and keeping it would
// refuse the new enrolling code as "invalid" — a message pointing nowhere.
func TestReEnrollingInTheSameWindowWorks(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	now := alignToStep(t)
	secret := enrollTOTP(t, ts.URL, admin, now)
	if rr := post(t, admin, ts.URL+"/api/me/2fa/disable",
		map[string]string{"code": codeForStep(t, secret, now+1)}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("disable = %d", rr.StatusCode)
	}
	// Same window, brand new secret: enrolling again must be accepted.
	r := post(t, admin, ts.URL+"/api/me/2fa/setup", nil)
	var again struct{ Secret string }
	json.NewDecoder(r.Body).Decode(&again)
	r.Body.Close()
	if again.Secret == secret {
		t.Fatal("re-enrollment should mint a new secret")
	}
	if rr := post(t, admin, ts.URL+"/api/me/2fa/enable",
		map[string]string{"code": codeForStep(t, again.Secret, now)}); rr.StatusCode != http.StatusOK {
		t.Errorf("re-enrolling in the same window = %d, want 200", rr.StatusCode)
	}
}
