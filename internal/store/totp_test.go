package store

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

func TestTOTPLifecycle(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "alice", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Pending secret is encrypted and reversible, 2FA not yet enabled.
	secret, _ := totp.GenerateSecret()
	if err := s.SetUserTOTPSecret(u.ID, secret); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	got, _ := s.GetUser(u.ID)
	if got.TOTPEnabled {
		t.Error("2FA should not be enabled before confirmation")
	}
	if got.TOTPSecretEnc == "" || got.TOTPSecretEnc == secret {
		t.Error("secret must be stored encrypted, not in clear text")
	}
	if dec, err := s.UserTOTPSecret(got); err != nil || dec != secret {
		t.Errorf("decrypt secret = %q, %v; want %q", dec, err, secret)
	}

	// Enable with recovery hashes.
	plain, hashes, _ := totp.NewRecoveryCodes(3)
	if err := s.EnableUserTOTP(u.ID, hashes); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ = s.GetUser(u.ID)
	if !got.TOTPEnabled || len(got.RecoveryCodes) != 3 {
		t.Fatalf("after enable: enabled=%v codes=%d", got.TOTPEnabled, len(got.RecoveryCodes))
	}

	// Consume a recovery code once; a second use fails.
	ok, err := s.ConsumeRecoveryCode(u.ID, plain[0])
	if err != nil || !ok {
		t.Fatalf("first consume = %v, %v; want true", ok, err)
	}
	if ok, _ := s.ConsumeRecoveryCode(u.ID, plain[0]); ok {
		t.Error("a recovery code must be single-use")
	}
	got, _ = s.GetUser(u.ID)
	if len(got.RecoveryCodes) != 2 {
		t.Errorf("remaining codes = %d, want 2", len(got.RecoveryCodes))
	}

	// Disable clears everything.
	if err := s.DisableUserTOTP(u.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ = s.GetUser(u.ID)
	if got.TOTPEnabled || got.TOTPSecretEnc != "" || len(got.RecoveryCodes) != 0 {
		t.Errorf("after disable: enabled=%v secret=%q codes=%d", got.TOTPEnabled, got.TOTPSecretEnc, len(got.RecoveryCodes))
	}
}
