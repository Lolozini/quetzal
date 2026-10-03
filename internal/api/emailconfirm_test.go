package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

func confirmTokenFrom(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "#confirm-email=")
	if i < 0 {
		t.Fatalf("no confirmation link in mail body:\n%s", body)
	}
	tok := body[i+len("#confirm-email="):]
	return strings.FieldsFunc(tok, func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' })[0]
}

func userFromBody(t *testing.T, r *http.Response) models.User {
	t.Helper()
	defer r.Body.Close()
	var u models.User
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		t.Fatal(err)
	}
	return u
}

func mailReady(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.SetSMTPConfig(map[string]string{"host": "smtp.example", "from": "noreply@example"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingPublicURL, "https://panel.example"); err != nil {
		t.Fatal(err)
	}
}

// A new address waits for its owner to open the link mailed to it; the account
// keeps the old one, which resets go to, until then.
func TestANewAddressIsTakenOnceConfirmed(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	mailReady(t, st)
	makeUser(t, st, "alice", "alicepw12", "old@example.com")
	if r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"}); r.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", r.StatusCode)
	}

	r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "new@example.com"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("set email = %d", r.StatusCode)
	}
	u := userFromBody(t, r)
	if u.Email != "old@example.com" || u.PendingEmail != "new@example.com" {
		t.Fatalf("after asking: email %q, pending %q; want the old one kept and the new one pending", u.Email, u.PendingEmail)
	}
	msgs := cap.waitFor(1)
	if len(msgs) != 1 || msgs[0].to[0] != "new@example.com" {
		t.Fatalf("mails = %+v, want one to the new address", msgs)
	}
	token := confirmTokenFrom(t, msgs[0].body)

	// Nobody signed in: the link may be opened anywhere.
	jar, _ := cookiejar.New(nil)
	anon := &http.Client{Jar: jar}
	if r := post(t, anon, ts.URL+"/api/confirm-email", map[string]string{"token": token}); r.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d", r.StatusCode)
	}
	got, _ := st.GetUserByUsername("alice")
	if got.Email != "new@example.com" || !got.EmailVerified || got.PendingEmail != "" {
		t.Fatalf("after confirming: %+v", got)
	}
	if r := post(t, anon, ts.URL+"/api/confirm-email", map[string]string{"token": token}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("the link a second time = %d, want 400", r.StatusCode)
	}

	// A change of mind: the link of an address the account no longer asks
	// for is dead.
	put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "third@example.com"})
	msgs = cap.waitFor(2)
	stale := confirmTokenFrom(t, msgs[1].body)
	if r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "new@example.com"}); r.StatusCode != http.StatusOK || userFromBody(t, r).PendingEmail != "" {
		t.Fatalf("asking for the current address again should drop the pending one")
	}
	if r := post(t, anon, ts.URL+"/api/confirm-email", map[string]string{"token": stale}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("a cancelled address's link = %d, want 400", r.StatusCode)
	}
	got, _ = st.GetUserByUsername("alice")
	if got.Email != "new@example.com" || !got.EmailVerified {
		t.Errorf("a dead link changed the account: %+v", got)
	}
}

// Two accounts may wait on one address; the first to confirm it has it.
func TestAnAddressTakenMeanwhileIsRefused(t *testing.T) {
	ts, _, st, cap := newResetHarness(t)
	mailReady(t, st)
	makeUser(t, st, "alice", "alicepw12", "")
	makeUser(t, st, "bob", "bobpw1234", "")
	alice := loginAs(t, ts.URL, "alice", "alicepw12")
	bob := loginAs(t, ts.URL, "bob", "bobpw1234")
	put(t, alice, ts.URL+"/api/me/email", map[string]string{"email": "shared@example.com"})
	put(t, bob, ts.URL+"/api/me/email", map[string]string{"email": "shared@example.com"})
	msgs := cap.waitFor(2)
	if r := post(t, bob, ts.URL+"/api/confirm-email", map[string]string{"token": confirmTokenFrom(t, msgs[1].body)}); r.StatusCode != http.StatusOK {
		t.Fatalf("bob confirms = %d", r.StatusCode)
	}
	if r := post(t, alice, ts.URL+"/api/confirm-email", map[string]string{"token": confirmTokenFrom(t, msgs[0].body)}); r.StatusCode != http.StatusConflict {
		t.Errorf("alice confirms bob's address = %d, want 409", r.StatusCode)
	}
}

// Without mail there is nothing to confirm by: the address is taken as given,
// unconfirmed, and can be confirmed once the panel can send.
func TestWithoutMailAnAddressIsTakenUnconfirmed(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	makeUser(t, st, "alice", "alicepw12", "")
	post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "alicepw12"})
	r := put(t, c, ts.URL+"/api/me/email", map[string]string{"email": "a@example.com"})
	if u := userFromBody(t, r); u.Email != "a@example.com" || u.EmailVerified || u.PendingEmail != "" {
		t.Fatalf("without mail: %+v", u)
	}
	if r := post(t, c, ts.URL+"/api/me/email/confirmation", nil); r.StatusCode != http.StatusConflict {
		t.Errorf("resend without mail = %d, want 409", r.StatusCode)
	}

	mailReady(t, st)
	if r := post(t, c, ts.URL+"/api/me/email/confirmation", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("resend = %d", r.StatusCode)
	}
	msgs := cap.waitFor(1)
	if len(msgs) != 1 || msgs[0].to[0] != "a@example.com" {
		t.Fatalf("mails = %+v", msgs)
	}
	post(t, c, ts.URL+"/api/confirm-email", map[string]string{"token": confirmTokenFrom(t, msgs[0].body)})
	if got, _ := st.GetUserByUsername("alice"); !got.EmailVerified {
		t.Errorf("the account's own address was not confirmed: %+v", got)
	}
	if r := post(t, c, ts.URL+"/api/me/email/confirmation", nil); r.StatusCode != http.StatusConflict {
		t.Errorf("resend with nothing left to confirm = %d, want 409", r.StatusCode)
	}
}

// An address an administrator writes is unconfirmed, and drops one the
// account was waiting on.
func TestAnAdminWrittenAddressIsUnconfirmed(t *testing.T) {
	_, _, st, _ := newResetHarness(t)
	u := makeUser(t, st, "alice", "alicepw12", "")
	if err := st.StartEmailConfirmation(&models.EmailConfirmation{UserID: u.ID, Email: "p@example.com", TokenHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateUserEmail(u.ID, "admin-set@example.com"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetUser(u.ID)
	if got.EmailVerified || got.PendingEmail != "" || got.Email != "admin-set@example.com" {
		t.Fatalf("after an admin write: %+v", got)
	}
	if _, err := st.ConfirmEmail("h"); err == nil {
		t.Error("the dropped address's link still works")
	}
}
