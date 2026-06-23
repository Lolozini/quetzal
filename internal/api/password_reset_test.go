package api_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

type sentMail struct {
	to      []string
	subject string
	body    string
}

type captureMailer struct {
	mu   sync.Mutex
	msgs []sentMail
}

func (c *captureMailer) send(_ context.Context, _ map[string]string, to []string, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, sentMail{to, subject, body})
	return nil
}

func (c *captureMailer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// waitFor polls until at least n messages were captured (sends are async).
func (c *captureMailer) waitFor(n int) []sentMail {
	for i := 0; i < 200; i++ {
		if c.count() >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sentMail(nil), c.msgs...)
}

func newResetHarness(t *testing.T) (*httptest.Server, *http.Client, *store.Store, *captureMailer) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "r.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := api.New(st, fake.NewSimpleClientset(), &rest.Config{})
	cap := &captureMailer{}
	srv.Mailer = cap.send
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}, st, cap
}

func makeUser(t *testing.T, st *store.Store, username, password, email string) *models.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := &models.User{Username: username, PasswordHash: hash, Email: email}
	if err := st.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func tokenFromBody(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "reset=")
	if i < 0 {
		t.Fatalf("no reset token in mail body:\n%s", body)
	}
	tok := body[i+len("reset="):]
	tok = strings.FieldsFunc(tok, func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' })[0]
	return tok
}

func TestPasswordResetFlow(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	if err := st.SetSMTPConfig(map[string]string{"host": "smtp.example", "from": "noreply@example"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingPublicURL, "https://panel.example"); err != nil {
		t.Fatal(err)
	}
	makeUser(t, st, "alice", "originalpw1", "alice@example.com")

	// Log alice in: this session must be killed by the reset.
	if r := post(t, c, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "originalpw1"}); r.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", r.StatusCode)
	}
	if r, _ := c.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusOK {
		t.Fatal("me should be 200 after login")
	}

	// Forgot by username triggers exactly one mail.
	if r := post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "alice"}); r.StatusCode != http.StatusOK {
		t.Fatalf("forgot = %d", r.StatusCode)
	}
	msgs := cap.waitFor(1)
	if len(msgs) != 1 {
		t.Fatalf("got %d mails, want 1", len(msgs))
	}
	if msgs[0].to[0] != "alice@example.com" {
		t.Errorf("mail to = %v", msgs[0].to)
	}
	if !strings.Contains(msgs[0].body, "https://panel.example/?reset=") {
		t.Errorf("body missing reset link:\n%s", msgs[0].body)
	}
	token := tokenFromBody(t, msgs[0].body)

	// Reset with the token.
	if r := post(t, c, ts.URL+"/api/reset-password", map[string]string{"token": token, "password": "brandnewpw1"}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", r.StatusCode)
	}

	// The old session is gone.
	if r, _ := c.Get(ts.URL + "/api/me"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after reset = %d, want 401", r.StatusCode)
	}
	// Old password no longer works; new one does.
	jar, _ := cookiejar.New(nil)
	fresh := &http.Client{Jar: jar}
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "originalpw1"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with old pw = %d, want 401", r.StatusCode)
	}
	if r := post(t, fresh, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "brandnewpw1"}); r.StatusCode != http.StatusOK {
		t.Errorf("login with new pw = %d, want 200", r.StatusCode)
	}

	// The token is single-use.
	if r := post(t, fresh, ts.URL+"/api/reset-password", map[string]string{"token": token, "password": "thirdpw12"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("token reuse = %d, want 400", r.StatusCode)
	}
}

func TestPasswordResetNoEnumeration(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	_ = st.SetSMTPConfig(map[string]string{"host": "smtp.example", "from": "noreply@example"})
	_ = st.SetSetting(store.SettingPublicURL, "https://panel.example")
	makeUser(t, st, "bob", "password12", "")          // no email
	makeUser(t, st, "carol", "password12", "c@x.com") // has email

	// Unknown identifier: 200, no mail.
	if r := post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "ghost"}); r.StatusCode != http.StatusOK {
		t.Fatalf("forgot unknown = %d", r.StatusCode)
	}
	// Known user without an email: 200, no mail.
	if r := post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "bob"}); r.StatusCode != http.StatusOK {
		t.Fatalf("forgot no-email = %d", r.StatusCode)
	}
	// Give the async path a chance, then assert nothing was sent.
	time.Sleep(50 * time.Millisecond)
	if n := cap.count(); n != 0 {
		t.Errorf("sent %d mails for unknown/no-email, want 0", n)
	}

	// Known user with email: 200 + exactly one mail (by email this time).
	if r := post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "c@x.com"}); r.StatusCode != http.StatusOK {
		t.Fatalf("forgot by email = %d", r.StatusCode)
	}
	if msgs := cap.waitFor(1); len(msgs) != 1 {
		t.Fatalf("got %d mails, want 1", len(msgs))
	}
}

func TestPasswordResetUnconfigured(t *testing.T) {
	ts, c, st, cap := newResetHarness(t)
	makeUser(t, st, "dave", "password12", "dave@x.com")
	// No SMTP / public URL configured at all.
	if r := post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "dave"}); r.StatusCode != http.StatusOK {
		t.Fatalf("forgot = %d", r.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if n := cap.count(); n != 0 {
		t.Errorf("sent %d mails while unconfigured, want 0", n)
	}
}

func TestPasswordResetInvalidToken(t *testing.T) {
	ts, c, st, _ := newResetHarness(t)
	u := makeUser(t, st, "erin", "password12", "erin@x.com")

	// Garbage token.
	if r := post(t, c, ts.URL+"/api/reset-password", map[string]string{"token": "nope", "password": "newpassword1"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("garbage token = %d, want 400", r.StatusCode)
	}
	// Weak password with an otherwise-missing token.
	if r := post(t, c, ts.URL+"/api/reset-password", map[string]string{"token": "x", "password": "short"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("weak password = %d, want 400", r.StatusCode)
	}
	// Expired token is rejected.
	if err := st.CreatePasswordReset(&models.PasswordReset{
		UserID: u.ID, TokenHash: api.HashTokenForTest("expiredtok"), ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if r := post(t, c, ts.URL+"/api/reset-password", map[string]string{"token": "expiredtok", "password": "newpassword1"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("expired token = %d, want 400", r.StatusCode)
	}
}
