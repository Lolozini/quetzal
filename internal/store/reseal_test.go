package store

import (
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/testdb"
)

// Until 0.19.0 a QUETZAL_SECRET_KEY of 64 hexadecimal characters was taken for no
// key: the instance wrote its secrets in clear text, behind "plain:". The key is
// accepted now, and what was written before has to be sealed, or it stays readable
// by anyone with the database or a backup of it and nothing says so.
func TestSecretsWrittenWithoutAKeyAreSealedOnceThereIsOne(t *testing.T) {
	dsn := testdb.DSN(t, "reseal.db")
	open := func(key []byte) *Store {
		t.Helper()
		st, err := Open(Config{Driver: Driver(testdb.Driver()), DSN: dsn, Silent: true, SecretKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Before: the hexadecimal key is read as no key.
	before := open(nil)
	if err := before.SetSMTPConfig(map[string]string{"host": "smtp.example.org", "password": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	ch := &models.NotificationChannel{Name: "hook", Type: models.ChannelWebhook, Enabled: true}
	if err := before.CreateChannel(ch, map[string]string{"url": "https://hooks.example.org/SECRET"}); err != nil {
		t.Fatal(err)
	}
	if err := before.SaveBackupConfig(&models.BackupConfig{Endpoint: "s3.example.org", Bucket: "b"}, "AKIA-ACCESS", "s3cr3t-key", "repo-pass"); err != nil {
		t.Fatal(err)
	}

	// After: the same variable is accepted.
	t.Setenv("QUETZAL_SECRET_KEY", strings.Repeat("ab", 32))
	key, err := crypto.KeyFromEnv("QUETZAL_SECRET_KEY")
	if err != nil || len(key) != 32 {
		t.Fatalf("the hexadecimal key was refused: %v", err)
	}
	after := open(key)

	smtp, err := after.GetSMTPConfig()
	if err != nil || smtp["password"] != "hunter2" {
		t.Fatalf("the SMTP password is unreadable after sealing: %v %v", smtp, err)
	}
	cfg, err := after.ChannelConfig(ch)
	if err != nil || cfg["url"] != "https://hooks.example.org/SECRET" {
		t.Fatalf("the channel's settings are unreadable after sealing: %v %v", cfg, err)
	}
	bc, err := after.GetBackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	access, secret, repo, err := after.BackupSecrets(bc)
	if err != nil || access != "AKIA-ACCESS" || secret != "s3cr3t-key" || repo != "repo-pass" {
		t.Fatalf("the backup credentials are unreadable after sealing: %q %q %q %v", access, secret, repo, err)
	}

	var clear []string
	for _, c := range sealedColumns {
		var n int64
		if err := after.db.Model(c.model).Where(c.column+" LIKE ?", secretPrefixPlain+"%").Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			clear = append(clear, c.column)
		}
	}
	if blob, _ := after.GetSetting(SettingSMTP); strings.HasPrefix(blob, secretPrefixPlain) {
		clear = append(clear, "settings."+SettingSMTP)
	}
	if len(clear) > 0 {
		t.Fatalf("still stored in clear text once the instance has a key: %v", clear)
	}

	// A second start has nothing left to seal and writes nothing.
	if n := after.resealPlainSecrets(); n != 0 {
		t.Errorf("a second pass sealed %d values, want 0", n)
	}
}

// Without a key there is nothing to seal with: the values stay as they are, and the
// instance keeps reading them.
func TestResealDoesNothingWithoutAKey(t *testing.T) {
	st := newTestStore(t) // no key
	if err := st.SetSMTPConfig(map[string]string{"password": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	if n := st.resealPlainSecrets(); n != 0 {
		t.Fatalf("sealed %d values with no key", n)
	}
	if cfg, err := st.GetSMTPConfig(); err != nil || cfg["password"] != "hunter2" {
		t.Fatalf("the value was changed: %v %v", cfg, err)
	}
}
