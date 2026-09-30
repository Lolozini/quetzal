package api_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Several changes that matter to whoever investigates an incident wrote nothing
// to the audit log: pointing every backup at another bucket, rewriting what a
// schedule runs (its creation and deletion were recorded, its edits not), an
// account's email, which password resets go to, its password, and the
// revocation of its API and SSH keys, whose creation was recorded.
func TestSensitiveChangesAreAudited(t *testing.T) {
	ts, admin, _ := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	if rr := doPut(t, admin, ts.URL+"/api/backup-config", map[string]any{
		"endpoint": "s3.example.com", "bucket": "elsewhere", "prefix": "games",
		"accessKey": "AK", "secretKey": "SK", "repoPassword": "RP",
	}); rr.StatusCode >= 300 {
		t.Fatalf("backup target = %d", rr.StatusCode)
	}

	var srv struct{ ID uint }
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process"})
	json.NewDecoder(r.Body).Decode(&srv)
	url := ts.URL + "/api/servers/" + itoa(srv.ID)
	var sc struct{ ID uint }
	r = post(t, admin, url+"/schedules", map[string]any{"name": "nightly", "cron": "0 4 * * *", "action": "command", "payload": "save-all", "enabled": true})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("schedule = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&sc)
	if rr := doPatch(t, admin, url+"/schedules/"+itoa(sc.ID), map[string]any{"action": "command", "payload": "op mallory"}); rr.StatusCode != http.StatusOK {
		t.Fatalf("edit schedule = %d", rr.StatusCode)
	}

	if rr := doPut(t, admin, ts.URL+"/api/me/email", map[string]string{"email": "someone@elsewhere.example"}); rr.StatusCode != http.StatusOK {
		t.Fatalf("email = %d", rr.StatusCode)
	}
	if rr := post(t, admin, ts.URL+"/api/me/password", map[string]string{"oldPassword": "supersecret", "newPassword": "anothersecret"}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("password = %d", rr.StatusCode)
	}

	var key struct{ Key struct{ ID uint } }
	r = post(t, admin, ts.URL+"/api/apikeys", map[string]string{"name": "ci"})
	json.NewDecoder(r.Body).Decode(&key)
	if rr := doMethod(t, admin, http.MethodDelete, ts.URL+"/api/apikeys/"+itoa(key.Key.ID), nil); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke API key = %d", rr.StatusCode)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	sp, _ := ssh.NewPublicKey(pub)
	var sshKey struct {
		ID          uint
		Fingerprint string
	}
	r = post(t, admin, ts.URL+"/api/me/sshkeys", map[string]string{"name": "laptop", "publicKey": string(ssh.MarshalAuthorizedKey(sp))})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("add SSH key = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&sshKey)
	if rr := doMethod(t, admin, http.MethodDelete, ts.URL+"/api/me/sshkeys/"+itoa(sshKey.ID), nil); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke SSH key = %d", rr.StatusCode)
	}

	var log []models.AuditEntry
	getJSON(t, admin, ts.URL+"/api/audit", &log)
	want := map[string]string{
		"backup.settings.update": "s3.example.com/elsewhere/games (new credentials, new repository password)",
		"schedule.update":        `nightly (command "op mallory" @ 0 4 * * *)`,
		"user.email":             "someone@elsewhere.example",
		"user.password":          "admin",
		"apikey.delete":          "ci",
		"sshkey.delete":          sshKey.Fingerprint,
	}
	for action, detail := range want {
		found := false
		for _, e := range log {
			if e.Action == action {
				found = true
				if e.Detail != detail || e.Username != "admin" {
					t.Errorf("%s recorded as %q by %q, want %q by admin", action, e.Detail, e.Username, detail)
				}
			}
		}
		if !found {
			t.Errorf("%s left no trace in the audit log", action)
		}
	}
	for _, e := range log {
		if strings.Contains(e.Detail, "SK") || strings.Contains(e.Detail, "RP") {
			t.Errorf("a secret reached the audit log: %s %q", e.Action, e.Detail)
		}
	}
}

// A password reset through the emailed link has no session to record it
// under; its entry goes under the account the link was for.
func TestPasswordResetIsAudited(t *testing.T) {
	ts, c, st, mail := newResetHarness(t)
	if err := st.SetSMTPConfig(map[string]string{"host": "smtp.example", "from": "noreply@example"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.SettingPublicURL, "https://panel.example"); err != nil {
		t.Fatal(err)
	}
	makeUser(t, st, "alice", "alicepw12", "alice@example.com")
	post(t, c, ts.URL+"/api/forgot-password", map[string]string{"identifier": "alice"})
	msgs := mail.waitFor(1)
	if len(msgs) == 0 {
		t.Fatal("no reset email")
	}
	if rr := post(t, c, ts.URL+"/api/reset-password", map[string]string{
		"token": tokenFromBody(t, msgs[0].body), "password": "brandnewpw",
	}); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("reset = %d", rr.StatusCode)
	}
	es, err := st.ListAudit(0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Action == "user.password-reset" {
			if e.Username != "alice" {
				t.Errorf("reset recorded under %q, want alice", e.Username)
			}
			return
		}
	}
	t.Error("a password reset left no trace in the audit log")
}
