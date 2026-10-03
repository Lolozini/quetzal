package authkeys

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func pubKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " me@laptop"
}

// A key comes back with its accounts, a name with odd characters in it (an
// account made before names were kept readable) included, and one key on two
// accounts with both.
func TestKeysCarryTheirAccounts(t *testing.T) {
	a, b := pubKey(t), pubKey(t)
	file := strings.Join([]string{
		Line(a, []string{"Lolozini"}),
		Line(b, []string{`qa user, "quoted"`}),
		Line(a, []string{"alice"}),
		Line("not a key", []string{"x"}),
	}, "\n")
	keys := Parse([]byte(file))
	if len(keys) != 2 {
		t.Fatalf("%d keys, want 2: %q", len(keys), file)
	}
	if !keys[0].Allows("lolozini") || !keys[0].Allows("alice") || keys[0].Allows("bob") {
		t.Errorf("key a lets in %v", keys[0].Users)
	}
	if !keys[1].Allows(`qa user, "quoted"`) || keys[1].Allows("qa user") {
		t.Errorf("key b lets in %v", keys[1].Users)
	}
	// A file from before keys carried accounts lets a key in under any name.
	if old := Parse([]byte(a + "\n")); len(old) != 1 || !old[0].Allows("anyone") {
		t.Errorf("an old file: %+v", old)
	}
}
