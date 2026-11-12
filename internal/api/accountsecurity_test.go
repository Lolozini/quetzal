package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

// An API key is a long-lived token for scripts and carries no second factor. One
// that leaks used to be enough to take the account: it changed the address password
// resets go to, then made other keys, so that revoking the first no longer ended the
// access. These need a signed-in session.
func TestAnAPIKeyCannotTakeTheAccountOver(t *testing.T) {
	srv, admin := newTestServer(t)
	setupAdmin(t, srv.URL, admin)
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepass1"})
	alice := loginAs(t, srv.URL, "alice", "alicepass1")

	r := post(t, alice, srv.URL+"/api/apikeys", map[string]string{"name": "bot"})
	var made struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&made); err != nil || made.Token == "" {
		t.Fatalf("a session could not make an API key: %v (%d)", err, r.StatusCode)
	}
	// Whoever found the key has no cookie, only the key.
	withKey := func(method, path string, body any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, srv.URL+path, &buf)
		req.Header.Set("Authorization", "Bearer "+made.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	for _, c := range []struct {
		method, path string
		body         any
	}{
		// The right password does not open these to a key: it is a session's to answer.
		{http.MethodPut, "/api/me/email", map[string]string{"email": "attacker@example.net", "password": "alicepass1"}},
		{http.MethodPost, "/api/me/email/confirmation", nil},
		{http.MethodDelete, "/api/me/email/pending", nil},
		{http.MethodPost, "/api/me/password", map[string]string{"oldPassword": "alicepass1", "newPassword": "alicepass2"}},
		{http.MethodPost, "/api/me/2fa/setup", nil},
		{http.MethodPost, "/api/me/2fa/enable", map[string]string{"code": "000000"}},
		{http.MethodPost, "/api/me/2fa/disable", map[string]string{"code": "000000"}},
		{http.MethodPost, "/api/me/sshkeys", map[string]string{"name": "k", "publicKey": "ssh-ed25519 AAAA"}},
		{http.MethodDelete, "/api/me/sshkeys/1", nil},
		{http.MethodPost, "/api/apikeys", map[string]string{"name": "second"}},
		{http.MethodDelete, "/api/apikeys/1", nil},
	} {
		if got := withKey(c.method, c.path, c.body); got != http.StatusForbidden {
			t.Errorf("%s %s with an API key = %d, want 403", c.method, c.path, got)
		}
	}
	// What a script does with a key is untouched.
	for _, path := range []string{"/api/me", "/api/apikeys", "/api/servers"} {
		if got := withKey(http.MethodGet, path, nil); got != http.StatusOK {
			t.Errorf("GET %s with an API key = %d, want 200", path, got)
		}
	}
	// And a signed-in session still can, with its password.
	if r := put(t, alice, srv.URL+"/api/me/email", map[string]string{"email": "alice@example.com", "password": "alicepass1"}); r.StatusCode != http.StatusOK {
		t.Errorf("a session changing its own address = %d, want 200", r.StatusCode)
	}
	// A key that is not one is not told apart from no key.
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/me/email", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer qk_not-a-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unknown key = %v %v, want 401", resp, err)
	}
}

// Changing the address password resets go to decides who owns the account. It is
// asked of whoever signed in, with the password and, if the account has one, the
// two-factor code, so that a stolen session is not a stolen account.
func TestChangingTheAddressNeedsThePassword(t *testing.T) {
	ts, c, st, _ := newResetHarness(t)
	makeUser(t, st, "alice", "alicepw12", "")
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"})

	for name, body := range map[string]map[string]string{
		"no password":    {"email": "a@example.com"},
		"wrong password": {"email": "a@example.com", "password": "nope"},
	} {
		if r := put(t, c, ts.URL+"/api/me/email", body); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, r.StatusCode)
		}
	}
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "a@example.com", "password": "alicepw12"}); r.StatusCode != http.StatusOK {
		t.Fatalf("the right password = %d, want 200", r.StatusCode)
	}
	// Asking for the address the account already has changes nothing, and asks for nothing.
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "a@example.com"}); r.StatusCode != http.StatusOK {
		t.Errorf("keeping the address = %d, want 200", r.StatusCode)
	}
	// Removing it is a change like another.
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": ""}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("removing the address without the password = %d, want 401", r.StatusCode)
	}
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "", "password": "alicepw12"}); r.StatusCode != http.StatusOK {
		t.Errorf("removing the address with the password = %d, want 200", r.StatusCode)
	}
}

func TestChangingTheAddressNeedsTheCodeWhenTwoFactorIsOn(t *testing.T) {
	ts, c, st, _ := newResetHarness(t)
	u := makeUser(t, st, "alice", "alicepw12", "")
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"})
	if err := st.EnableUserTOTP(u.ID, []string{totp.HashRecovery("recovery")}); err != nil {
		t.Fatal(err)
	}
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "a@example.com", "password": "alicepw12"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("the password without the code = %d, want 400", r.StatusCode)
	}
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "a@example.com", "password": "alicepw12", "code": "recovery"}); r.StatusCode != http.StatusOK {
		t.Errorf("the password and a code = %d, want 200", r.StatusCode)
	}
}

// The confirmation goes to the new address. The old one, the one resets still go to
// until then, used to hear nothing of it.
func TestTheOldAddressIsToldOfAChange(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	mailReady(t, st)
	makeUser(t, st, "alice", "alicepw12", "old@example.com")
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"})

	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "new@example.com", "password": "alicepw12"}); r.StatusCode != http.StatusOK {
		t.Fatalf("change = %d", r.StatusCode)
	}
	notified := func(to string, msgs []sentMail) *sentMail {
		for i := range msgs {
			if len(msgs[i].to) == 1 && msgs[i].to[0] == to {
				return &msgs[i]
			}
		}
		return nil
	}
	msgs := cap.waitFor(2)
	if notified("new@example.com", msgs) == nil {
		t.Error("the new address got no confirmation")
	}
	n := notified("old@example.com", msgs)
	if n == nil {
		t.Fatalf("the old address was not told: %d mails to %v", len(msgs), msgs)
	}
	if !strings.Contains(n.body, "new@example.com") || !strings.Contains(n.subject, "changed") {
		t.Errorf("the notice does not say what is changing: %q\n%s", n.subject, n.body)
	}
	if strings.Contains(n.body, "#confirm-email=") {
		t.Error("the notice to the old address carries a confirmation link")
	}
}

// A reset is how an account is taken back, and a thief could have left API keys and
// SFTP keys behind: they outlive a password and a session.
func TestAPasswordResetEndsTheAPIKeysAndSSHKeys(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	mailReady(t, st)
	u := makeUser(t, st, "alice", "alicepw12", "alice@example.com")
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"})
	r := post(t, c, ts.URL+"/api/apikeys", map[string]string{"name": "bot"})
	var made struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&made); err != nil || made.Token == "" {
		t.Fatalf("api key: %v (%d)", err, r.StatusCode)
	}
	if err := st.AddSSHKey(&models.SSHKey{UserID: u.ID, Name: "laptop", PublicKey: "ssh-ed25519 AAAA", Fingerprint: "SHA256:test"}); err != nil {
		t.Fatal(err)
	}
	keyGet := func() int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+made.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if got := keyGet(); got != http.StatusOK {
		t.Fatalf("the key before the reset = %d", got)
	}

	post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "alice"})
	token := tokenFromBody(t, cap.waitFor(1)[0].body)
	if r := post(t, c, ts.URL+"/api/reset-password", map[string]string{"token": token, "password": "brandnewpw1"}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", r.StatusCode)
	}

	if got := keyGet(); got != http.StatusUnauthorized {
		t.Errorf("the API key after a reset = %d, want 401", got)
	}
	if keys, err := st.ListSSHKeysForUser(u.ID); err != nil || len(keys) != 0 {
		t.Errorf("SFTP keys after a reset: %d (%v), want none", len(keys), err)
	}
}
