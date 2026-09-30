package api_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The same public key could be added twice to an account, and deleting one of
// the two left the key working through the other: an SFTP session stayed open
// after its key was "revoked". A key is on an account once; another account may
// hold it too, since SFTP names the account it logs into.
func TestAnSSHKeyIsOnAnAccountOnce(t *testing.T) {
	ts, admin := newTestServer(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, ts.URL, "alice", "alicepw12")

	pub, _, _ := ed25519.GenerateKey(nil)
	sp, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
	add := func(c *http.Client, name string) (int, string) {
		t.Helper()
		r := post(t, c, ts.URL+"/api/me/sshkeys", map[string]string{"name": name, "publicKey": line + " " + name})
		var body struct {
			ID    uint   `json:"id"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return r.StatusCode, body.Error
	}

	if code, _ := add(admin, "dup1"); code != http.StatusCreated {
		t.Fatalf("first key = %d", code)
	}
	code, msg := add(admin, "dup2")
	if code != http.StatusConflict || !strings.Contains(msg, `"dup1"`) {
		t.Errorf("the same key again = %d %q, want 409 naming dup1", code, msg)
	}
	var keys []struct{ ID uint }
	r, _ := admin.Get(ts.URL + "/api/me/sshkeys")
	_ = json.NewDecoder(r.Body).Decode(&keys)
	if len(keys) != 1 {
		t.Fatalf("the account holds %d keys, want 1", len(keys))
	}
	if rr := doMethod(t, admin, http.MethodDelete, ts.URL+"/api/me/sshkeys/"+itoa(keys[0].ID), nil); rr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", rr.StatusCode)
	}
	r, _ = admin.Get(ts.URL + "/api/me/sshkeys")
	keys = nil
	_ = json.NewDecoder(r.Body).Decode(&keys)
	if len(keys) != 0 {
		t.Errorf("the key is still on the account after its deletion: %+v", keys)
	}

	if code, _ := add(alice, "shared"); code != http.StatusCreated {
		t.Errorf("another account adding the key = %d, want 201", code)
	}
}
