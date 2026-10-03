package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/ratelimit"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

type inviteHarness struct {
	ts    *httptest.Server
	st    *store.Store
	srv   *api.Server
	mail  *captureMailer
	admin *http.Client
	alice *http.Client // owns the server
	url   string       // the server's API URL
	id    uint
}

// newInviteHarness is a panel that can send email, with an administrator and
// alice, who owns one server.
func newInviteHarness(t *testing.T) *inviteHarness {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "i.db"), Silent: true})
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
	h := &inviteHarness{st: st, srv: srv, mail: &captureMailer{}}
	srv.Mailer = h.mail.send
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)
	if err := st.SetSMTPConfig(map[string]string{"host": "smtp.example", "from": "noreply@example"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingPublicURL, "https://panel.example"); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	h.admin = &http.Client{Jar: jar}
	setupAdmin(t, h.ts.URL, h.admin)
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	h.alice = loginAs(t, h.ts.URL, "alice", "alicepw12")
	r := post(t, h.alice, h.ts.URL+"/api/servers", map[string]any{"name": "Alice's world", "template": "generic-process", "memory": "512Mi"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	h.id = created.ID
	h.url = h.ts.URL + "/api/servers/" + itoa(created.ID)
	return h
}

// invite sends an invitation as c and returns the token from the mail it sent.
func (h *inviteHarness) invite(t *testing.T, c *http.Client, email string, perms ...string) string {
	t.Helper()
	before := h.mail.count()
	r := post(t, c, h.url+"/invites", map[string]any{"email": email, "permissions": perms})
	if r.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("invite %s = %d: %s", email, r.StatusCode, b)
	}
	msgs := h.mail.waitFor(before + 1)
	body := msgs[len(msgs)-1].body
	i := strings.Index(body, "https://panel.example/#invite=")
	if i < 0 {
		t.Fatalf("no invitation link in the mail:\n%s", body)
	}
	return strings.Fields(body[i+len("https://panel.example/#invite="):])[0]
}

func status(t *testing.T, r *http.Response) int {
	t.Helper()
	r.Body.Close()
	return r.StatusCode
}

func anon() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// An invitation goes to an address, and whoever reads that mailbox can make
// an account from it, which then reaches the server and nothing more.
func TestInvitationCreatesAnAccountWithAccess(t *testing.T) {
	h := newInviteHarness(t)
	tok := h.invite(t, h.alice, "Carol@Example.com", "view", "power")

	msgs := h.mail.waitFor(1)
	if got := msgs[0].to; len(got) != 1 || got[0] != "carol@example.com" {
		t.Errorf("mailed to %v", got)
	}
	for _, want := range []string{"alice invited you", "Alice's world", "View: its page", "Power: start"} {
		if !strings.Contains(msgs[0].body, want) {
			t.Errorf("the mail does not say %s:\n%s", want, msgs[0].body)
		}
	}
	// The HTML version escapes what the owner typed.
	if !strings.Contains(msgs[0].html, "Alice&#39;s world") || !strings.Contains(msgs[0].html, "#invite="+tok) {
		t.Errorf("the HTML version lacks the escaped name or the link")
	}

	// The owner sees it waiting, without its token.
	r, _ := h.alice.Get(h.url + "/invites")
	raw, _ := io.ReadAll(r.Body)
	if strings.Contains(string(raw), tok) || strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Errorf("the list gives the token away: %s", raw)
	}
	var open []models.ServerInvite
	json.Unmarshal(raw, &open)
	if len(open) != 1 || open[0].Email != "carol@example.com" || open[0].InvitedByName != "alice" {
		t.Fatalf("open invitations = %+v", open)
	}

	// The link says what it offers.
	carol := anon()
	r = post(t, carol, h.ts.URL+"/api/invites/inspect", map[string]string{"token": tok})
	var seen struct {
		Server, InvitedBy, Email string
		Permissions              []string
		Signup                   bool
	}
	json.NewDecoder(r.Body).Decode(&seen)
	if r.StatusCode != http.StatusOK || seen.Server != "Alice's world" || seen.InvitedBy != "alice" || !seen.Signup {
		t.Fatalf("inspect = %d %+v", r.StatusCode, seen)
	}

	r = post(t, carol, h.ts.URL+"/api/invites/register", map[string]string{"token": tok, "username": "carol", "password": "carolpw12"})
	if r.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("register = %d: %s", r.StatusCode, b)
	}
	// Signed in, and the server is in her list.
	var list []map[string]any
	getJSON(t, carol, h.ts.URL+"/api/servers", &list)
	if len(list) != 1 {
		t.Fatalf("carol sees %d servers, want 1", len(list))
	}
	u, err := h.st.GetUserByUsername("carol")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "carol@example.com" || u.IsAdmin || u.MaxServers != 0 {
		t.Errorf("new account = email %q admin %v maxServers %d; want the invited address, no admin, no servers of its own",
			u.Email, u.IsAdmin, u.MaxServers)
	}
	if !u.EmailVerified {
		t.Error("the invited address is not confirmed, though its link was opened")
	}
	a, err := h.st.GetServerAccess(h.id, u.ID)
	if err != nil || strings.Join(a.Permissions, ",") != "view,power" {
		t.Errorf("access = %+v, %v", a, err)
	}

	// Used up.
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/register",
		map[string]string{"token": tok, "username": "carol2", "password": "carolpw12"})); c != http.StatusNotFound {
		t.Errorf("second register = %d, want 404", c)
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": tok})); c != http.StatusNotFound {
		t.Errorf("inspect after use = %d, want 404", c)
	}
	if _, err := h.st.GetUserByUsername("carol2"); err == nil {
		t.Error("a used invitation still created an account")
	}
}

// Accepting from an account that already exists. An account that only lists
// the invited address in its profile gets nothing: that address was never
// checked, so it proves nothing.
func TestInvitationAcceptedFromAnExistingAccount(t *testing.T) {
	h := newInviteHarness(t)
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "bob", "password": "bobpw1234", "email": "bob@home.example"})
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "mallory", "password": "mallorypw1", "email": "bob@work.example"})
	bob := loginAs(t, h.ts.URL, "bob", "bobpw1234")
	mallory := loginAs(t, h.ts.URL, "mallory", "mallorypw1")

	tok := h.invite(t, h.alice, "bob@work.example", "view", "files")

	var list []map[string]any
	getJSON(t, mallory, h.ts.URL+"/api/servers", &list)
	if len(list) != 0 {
		t.Fatalf("an account with the invited address in its profile reached the server")
	}

	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/accept", map[string]string{"token": tok})); c != http.StatusUnauthorized {
		t.Errorf("accept without a session = %d, want 401", c)
	}
	r := post(t, bob, h.ts.URL+"/api/invites/accept", map[string]string{"token": tok})
	var out struct{ ServerID uint }
	json.NewDecoder(r.Body).Decode(&out)
	if r.StatusCode != http.StatusOK || out.ServerID != h.id {
		t.Fatalf("accept = %d %+v", r.StatusCode, out)
	}
	getJSON(t, bob, h.ts.URL+"/api/servers", &list)
	if len(list) != 1 {
		t.Errorf("bob sees %d servers, want 1", len(list))
	}
	if c := status(t, post(t, mallory, h.ts.URL+"/api/invites/accept", map[string]string{"token": tok})); c != http.StatusNotFound {
		t.Errorf("second accept = %d, want 404", c)
	}

	// The owner has nothing to accept.
	own := h.invite(t, h.alice, "alice@example.com", "view")
	if c := status(t, post(t, h.alice, h.ts.URL+"/api/invites/accept", map[string]string{"token": own})); c != http.StatusBadRequest {
		t.Errorf("owner accepting = %d, want 400", c)
	}
}

func TestInvitationRules(t *testing.T) {
	h := newInviteHarness(t)
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "bob", "password": "bobpw1234"})
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "eve", "password": "evepw1234"})
	bob := loginAs(t, h.ts.URL, "bob", "bobpw1234")
	eve := loginAs(t, h.ts.URL, "eve", "evepw1234")
	if c := status(t, post(t, h.alice, h.url+"/access", map[string]any{"username": "bob", "permissions": []string{"view"}})); c != http.StatusNoContent {
		t.Fatalf("grant bob = %d", c)
	}

	body := map[string]any{"email": "x@example.com", "permissions": []string{"view"}}
	if c := status(t, post(t, bob, h.url+"/invites", body)); c != http.StatusForbidden {
		t.Errorf("subuser inviting = %d, want 403", c)
	}
	if c := status(t, post(t, eve, h.url+"/invites", body)); c != http.StatusNotFound {
		t.Errorf("stranger inviting = %d, want 404", c)
	}
	if c := status(t, post(t, h.admin, h.url+"/invites", body)); c != http.StatusCreated {
		t.Errorf("admin inviting = %d, want 201", c)
	}

	for _, bad := range []string{"", "nope", "Name <x@example.com>", "x@example.com, y@example.com", "x@example.com\r\nBcc: y@example.com"} {
		if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": bad, "permissions": []string{"view"}})); c != http.StatusBadRequest {
			t.Errorf("email %q = %d, want 400", bad, c)
		}
	}
	if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": "x@example.com"})); c != http.StatusBadRequest {
		t.Errorf("no permission = %d, want 400", c)
	}
	if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": "x@example.com", "permissions": []string{"root"}})); c != http.StatusBadRequest {
		t.Errorf("unknown permission = %d, want 400", c)
	}

	// Inviting again retires the first link.
	first := h.invite(t, h.alice, "y@example.com", "view")
	second := h.invite(t, h.alice, "y@example.com", "view", "console")
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": first})); c != http.StatusNotFound {
		t.Errorf("first link after a second invitation = %d, want 404", c)
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": second})); c != http.StatusOK {
		t.Errorf("second link = %d, want 200", c)
	}

	// Withdrawing one kills its link.
	var open []models.ServerInvite
	getJSON(t, h.alice, h.url+"/invites", &open)
	for _, inv := range open {
		if inv.Email == "y@example.com" {
			if c := deleteAs(t, h.alice, h.url+"/invites/"+itoa(inv.ID)); c != http.StatusNoContent {
				t.Fatalf("withdraw = %d", c)
			}
		}
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": second})); c != http.StatusNotFound {
		t.Errorf("withdrawn link = %d, want 404", c)
	}

	// An expired link is no link.
	const old = "expired-token-0123456789"
	sum := sha256.Sum256([]byte(old))
	if err := h.st.SetServerInvite(&models.ServerInvite{
		ServerID: h.id, Email: "z@example.com", Permissions: []string{"view"}, InvitedBy: 1,
		TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/register",
		map[string]string{"token": old, "username": "zed", "password": "zedpw1234"})); c != http.StatusNotFound {
		t.Errorf("expired link = %d, want 404", c)
	}
}

// Nothing is stored when the mail does not leave, and nothing is sent when
// the panel cannot send mail at all.
func TestInvitationNeedsWorkingEmail(t *testing.T) {
	h := newInviteHarness(t)
	h.srv.Mailer = func(context.Context, map[string]string, []string, notify.Mail) error {
		return errors.New("relay said no")
	}
	if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": "x@example.com", "permissions": []string{"view"}})); c != http.StatusBadGateway {
		t.Errorf("mail failure = %d, want 502", c)
	}
	if n, _ := h.st.CountServerInvites(h.id); n != 0 {
		t.Errorf("%d invitations kept after the mail failed", n)
	}

	// An address the mail server refuses is the address's fault: the
	// recette of 0.10.0 invited x@localhost and was told to check the email
	// settings, which were fine (R-28).
	h.srv.Mailer = func(context.Context, map[string]string, []string, notify.Mail) error {
		return &notify.RecipientError{Addr: "x@localhost", Err: errors.New("550 5.1.1 <x@localhost>: Recipient address rejected")}
	}
	r := post(t, h.alice, h.url+"/invites", map[string]any{"email": "x@localhost", "permissions": []string{"view"}})
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "refused this address") || strings.Contains(string(body), "settings") {
		t.Errorf("a refused address = %d %s, want 400 blaming the address", r.StatusCode, body)
	}

	h.srv.Mailer = h.mail.send
	if err := h.st.SetSetting(store.SettingPublicURL, ""); err != nil {
		t.Fatal(err)
	}
	if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": "x@example.com", "permissions": []string{"view"}})); c != http.StatusConflict {
		t.Errorf("no public address = %d, want 409", c)
	}
}

func TestInvitationsAreRateLimited(t *testing.T) {
	h := newInviteHarness(t)
	h.srv.InviteLimiter = ratelimit.New(2, time.Hour)
	h.invite(t, h.alice, "a@example.com", "view")
	h.invite(t, h.alice, "b@example.com", "view")
	if c := status(t, post(t, h.alice, h.url+"/invites", map[string]any{"email": "c@example.com", "permissions": []string{"view"}})); c != http.StatusTooManyRequests {
		t.Errorf("third invitation in the window = %d, want 429", c)
	}
}

// An administrator can keep invitations to existing accounts. The link then
// says so, and accepting from an account still works.
func TestInvitationSignupCanBeTurnedOff(t *testing.T) {
	h := newInviteHarness(t)
	r := doMethod(t, h.admin, http.MethodPut, h.ts.URL+"/api/security-settings", map[string]any{"inviteSignup": false})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("turn off = %d", r.StatusCode)
	}
	// Naming one setting leaves the other alone: an empty requireTwoFactor
	// used to mean off, and this request has none.
	if v, _ := h.st.GetSetting(store.SettingRequire2FA); v != "" {
		t.Errorf("the second-factor policy was written (%q) by a request that did not name it", v)
	}

	tok := h.invite(t, h.alice, "dan@example.com", "view")
	if msgs := h.mail.waitFor(1); strings.Contains(msgs[len(msgs)-1].body, "create one") {
		t.Errorf("the mail offers an account the panel will not create:\n%s", msgs[len(msgs)-1].body)
	}
	r = post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": tok})
	var seen struct{ Signup bool }
	json.NewDecoder(r.Body).Decode(&seen)
	if seen.Signup {
		t.Error("inspect still offers to create an account")
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/register",
		map[string]string{"token": tok, "username": "dan", "password": "danpw1234"})); c != http.StatusForbidden {
		t.Errorf("register = %d, want 403", c)
	}
	createUser(t, h.admin, h.ts.URL, map[string]any{"username": "dan", "password": "danpw1234"})
	if c := status(t, post(t, loginAs(t, h.ts.URL, "dan", "danpw1234"), h.ts.URL+"/api/invites/accept", map[string]string{"token": tok})); c != http.StatusOK {
		t.Errorf("accept from an account = %d, want 200", c)
	}

	// A refused change changes nothing: requiring a second factor the admin
	// does not hold is refused, and the other field must not slip through.
	r = doMethod(t, h.admin, http.MethodPut, h.ts.URL+"/api/security-settings", map[string]any{"inviteSignup": true, "requireTwoFactor": "admins"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("refused change = %d, want 409", r.StatusCode)
	}
	if v, _ := h.st.GetSetting(store.SettingInviteSignup); v != "off" {
		t.Errorf("a refused request still turned account creation %q", v)
	}
}

// Invitations go with their server, and with the account that sent them.
func TestInvitationsGoWithTheirServerAndSender(t *testing.T) {
	h := newInviteHarness(t)
	tok := h.invite(t, h.alice, "a@example.com", "view")
	if c := deleteAs(t, h.alice, h.url); c != http.StatusNoContent && c != http.StatusAccepted {
		t.Fatalf("delete server = %d", c)
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": tok})); c != http.StatusNotFound {
		t.Errorf("link to a deleted server = %d, want 404", c)
	}

	r := post(t, h.alice, h.ts.URL+"/api/servers", map[string]any{"name": "second", "template": "generic-process", "memory": "512Mi"})
	var created struct{ ID uint }
	json.NewDecoder(r.Body).Decode(&created)
	h.url = h.ts.URL + "/api/servers/" + itoa(created.ID)
	tok = h.invite(t, h.alice, "b@example.com", "view")
	alice, err := h.st.GetUserByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	if c := deleteAs(t, h.admin, h.ts.URL+"/api/users/"+itoa(alice.ID)); c != http.StatusNoContent {
		t.Fatalf("delete alice = %d", c)
	}
	if c := status(t, post(t, anon(), h.ts.URL+"/api/invites/inspect", map[string]string{"token": tok})); c != http.StatusNotFound {
		t.Errorf("link from a deleted account = %d, want 404", c)
	}
}
