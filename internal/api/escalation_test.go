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
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

// relayMailer records which SMTP relay each message went through, which is what
// the escalation below turns on.
type relayMailer struct {
	mu   sync.Mutex
	cfgs []map[string]string
	body []string
}

func (m *relayMailer) send(_ context.Context, cfg map[string]string, _ []string, _, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfgs = append(m.cfgs, cfg)
	m.body = append(m.body, body)
	return nil
}

func (m *relayMailer) wait(t *testing.T) (map[string]string, string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		n := len(m.cfgs)
		m.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cfgs) == 0 {
		t.Fatal("no mail was sent")
	}
	return m.cfgs[0], m.body[0]
}

func newMailHarness(t *testing.T) (*httptest.Server, *http.Client, *relayMailer) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "e.db"), Silent: true})
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
	m := &relayMailer{}
	srv.Mailer = m.send
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}, m
}

// A scoped users-admin must not be able to strip two-factor from an account
// with admin standing. Updating and deleting such an account are both refused
// for the same reason -- it would hand over an account more privileged than the
// caller's -- and this endpoint carries the same permission.
func TestUsersAdminCannotStripAnAdminsTwoFactor(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	roleID := createRole(t, admin, srv.URL, "user-mgr", []string{models.AdminPermUsers})
	createUser(t, admin, srv.URL, map[string]any{"username": "uma", "password": "umapw1234"})
	setUserRole(t, admin, srv.URL, userID(t, admin, srv.URL, "uma"), &roleID)
	uma := loginAs(t, srv.URL, "uma", "umapw1234")

	adminID := userID(t, admin, srv.URL, "admin")
	if r := post(t, uma, srv.URL+"/api/users/"+itoa(adminID)+"/2fa/disable", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin disabling the superadmin's 2FA = %d, want 403", r.StatusCode)
	}

	// Same for a scoped admin, which the update and delete paths also protect.
	settingsRole := createRole(t, admin, srv.URL, "settings-only", []string{models.AdminPermSettings})
	createUser(t, admin, srv.URL, map[string]any{"username": "vic", "password": "vicpw1234"})
	vicID := userID(t, admin, srv.URL, "vic")
	setUserRole(t, admin, srv.URL, vicID, &settingsRole)
	if r := post(t, uma, srv.URL+"/api/users/"+itoa(vicID)+"/2fa/disable", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("users-admin disabling a scoped admin's 2FA = %d, want 403", r.StatusCode)
	}

	// A regular user is exactly what the permission is for.
	createUser(t, admin, srv.URL, map[string]any{"username": "reg", "password": "regpw1234"})
	regID := userID(t, admin, srv.URL, "reg")
	if r := post(t, uma, srv.URL+"/api/users/"+itoa(regID)+"/2fa/disable", nil); r.StatusCode != http.StatusNoContent {
		t.Errorf("users-admin disabling a regular user's 2FA = %d, want 204", r.StatusCode)
	}
}

// Whoever configures SMTP reads every mail the panel sends, and one of those is
// a password reset link. Left delegable, a settings-scoped admin could point the
// relay at itself, ask for the superadmin's reset and take the account -- this
// test walked that whole path and it worked. So the write is superadmin-only,
// and this asserts the chain stays broken at its first link.
func TestSettingsAdminCannotRepointTheMailRelay(t *testing.T) {
	srv, admin, mail := newMailHarness(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	doPut(t, admin, srv.URL+"/api/me/email", map[string]string{"email": "admin@example.com"})
	if r := doPut(t, admin, srv.URL+"/api/email-settings", map[string]string{
		"host": "smtp.legit", "from": "noreply@example", "publicUrl": "https://panel.example",
	}); r.StatusCode != http.StatusOK && r.StatusCode != http.StatusNoContent {
		t.Fatalf("superadmin set smtp = %d", r.StatusCode)
	}

	role := createRole(t, admin, srv.URL, "settings-only", []string{models.AdminPermSettings})
	createUser(t, admin, srv.URL, map[string]any{"username": "sam", "password": "sampw1234"})
	setUserRole(t, admin, srv.URL, userID(t, admin, srv.URL, "sam"), &role)
	sam := loginAs(t, srv.URL, "sam", "sampw1234")

	if r := doPut(t, sam, srv.URL+"/api/email-settings", map[string]string{
		"host": "smtp.attacker", "from": "noreply@example", "publicUrl": "https://panel.example",
	}); r.StatusCode != http.StatusForbidden {
		t.Errorf("settings-admin repointing the relay = %d, want 403", r.StatusCode)
	}
	// The public URL alone is enough: the reset link is built from it.
	if r := doPut(t, sam, srv.URL+"/api/email-settings", map[string]string{
		"host": "smtp.legit", "from": "noreply@example", "publicUrl": "https://evil.example",
	}); r.StatusCode != http.StatusForbidden {
		t.Errorf("settings-admin repointing the public URL = %d, want 403", r.StatusCode)
	}

	// Reading stays delegated, and comes back without the password.
	var got map[string]any
	getJSON(t, sam, srv.URL+"/api/email-settings", &got)
	if got["host"] != "smtp.legit" {
		t.Errorf("settings-admin reads host = %v, want smtp.legit", got["host"])
	}
	if _, leaked := got["password"]; leaked {
		t.Error("email settings returned the SMTP password")
	}

	// And the superadmin's reset still leaves through the relay the superadmin
	// chose, carrying a link to the panel's real address.
	anon := &http.Client{}
	post(t, anon, srv.URL+"/api/forgot-password", map[string]string{"identifier": "admin"})
	cfg, body := mail.wait(t)
	if cfg["host"] != "smtp.legit" {
		t.Errorf("reset went via %q, want smtp.legit", cfg["host"])
	}
	if !strings.Contains(body, "https://panel.example") {
		t.Errorf("reset link does not point at the panel:\n%s", body)
	}
}
