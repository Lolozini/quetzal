package store

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

func TestRecoveryCodeConcurrentConsumption(t *testing.T) {
	for _, same := range []bool{true, false} {
		name := "distinct"
		if same {
			name = "same"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			u := &models.User{Username: "alice", PasswordHash: "old"}
			if err := s.CreateUser(u); err != nil {
				t.Fatal(err)
			}
			codes, hashes, err := totp.NewRecoveryCodes(2)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.EnableUserTOTP(u.ID, hashes); err != nil {
				t.Fatal(err)
			}
			// Suspend the first read after it has released its DB connection.
			// Another consumer finishes before that stale snapshot is resumed.
			read, resume := make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			if err := s.db.Callback().Query().After("gorm:query").Register("test:recovery_snapshot", func(tx *gorm.DB) {
				if tx.Statement.Table == "users" && reads.Add(1) == 1 {
					close(read)
					<-resume
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.db.Callback().Query().Remove("test:recovery_snapshot") })
			type result struct {
				ok  bool
				err error
			}
			first := make(chan result, 1)
			go func() { ok, err := s.ConsumeRecoveryCode(u.ID, codes[0]); first <- result{ok, err} }()
			<-read
			other := codes[1]
			if same {
				other = codes[0]
			}
			ok, err := s.ConsumeRecoveryCode(u.ID, other)
			close(resume)
			a := <-first
			if err != nil || a.err != nil {
				t.Fatalf("consume errors: %v, %v", err, a.err)
			}
			if same && ok && a.ok {
				t.Error("same recovery code succeeded twice")
			}
			if !same && (!ok || !a.ok) {
				t.Error("distinct recovery codes must both succeed")
			}
			if !same {
				for _, code := range codes {
					if reused, err := s.ConsumeRecoveryCode(u.ID, code); reused || err != nil {
						t.Errorf("consumed code reintroduced: %v, %v", reused, err)
					}
				}
			}
		})
	}
}

func TestPasswordChangeInvalidatesSensitiveLinks(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "alice", PasswordHash: "old", Email: "old@example.com"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(&models.PasswordReset{UserID: u.ID, TokenHash: "reset", ExpiresAt: time.Now().Add(time.Hour)}, u); err != nil {
		t.Fatal(err)
	}
	if err := s.StartEmailConfirmation(&models.EmailConfirmation{UserID: u.ID, TokenHash: "confirm", Email: "new@example.com", ExpiresAt: time.Now().Add(time.Hour)}, u.PasswordHash); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserPassword(u.ID, "new", u.PasswordHash, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPasswordResetByHash("reset"); err == nil {
		t.Error("old reset still valid")
	}
	if _, err := s.ConfirmEmail("confirm"); err == nil {
		t.Error("old email confirmation still valid")
	}
	u, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.PendingEmail != "" || u.Email != "old@example.com" {
		t.Errorf("email state survived password change: %+v", u)
	}
}

func TestEmailChangeRevokesOldReset(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmation", false: "direct"}[confirmed], func(t *testing.T) {
			s := newTestStore(t)
			u := &models.User{Username: "alice", PasswordHash: "old", Email: "old@example.com"}
			if err := s.CreateUser(u); err != nil {
				t.Fatal(err)
			}
			if err := s.CreatePasswordReset(&models.PasswordReset{UserID: u.ID, TokenHash: "reset", ExpiresAt: time.Now().Add(time.Hour)}, u); err != nil {
				t.Fatal(err)
			}
			if confirmed {
				if err := s.StartEmailConfirmation(&models.EmailConfirmation{UserID: u.ID, TokenHash: "confirm", Email: "new@example.com", ExpiresAt: time.Now().Add(time.Hour)}, u.PasswordHash); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ConfirmEmail("confirm"); err != nil {
					t.Fatal(err)
				}
			} else if err := s.UpdateUserEmail(u.ID, "new@example.com"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetPasswordResetByHash("reset"); err == nil {
				t.Error("reset addressed to previous email remains valid")
			}
		})
	}
}

func TestPasswordMutationRollsBackRevocationFailure(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "alice", PasswordHash: "old"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(&models.PasswordReset{UserID: u.ID, TokenHash: "reset", ExpiresAt: time.Now().Add(time.Hour)}, u); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("session revocation unavailable")
	if err := s.db.Callback().Delete().Before("gorm:delete").Register("test:revocation_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Callback().Delete().Remove("test:revocation_failure") })
	if err := s.UpdateUserPassword(u.ID, "new", u.PasswordHash, ""); !errors.Is(err, failure) {
		t.Errorf("revocation failure not returned: %v", err)
	}
	current, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PasswordHash != "old" {
		t.Error("password committed despite revocation failure")
	}
	if _, err := s.GetPasswordResetByHash("reset"); err != nil {
		t.Errorf("reset removed despite rollback: %v", err)
	}
}

func TestResetRollsBackConsumptionOnRevocationFailure(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "alice", PasswordHash: "old"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	pr := &models.PasswordReset{UserID: u.ID, TokenHash: "reset", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreatePasswordReset(pr, u); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("session revocation unavailable")
	if err := s.db.Callback().Delete().Before("gorm:delete").Register("test:reset_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Callback().Delete().Remove("test:reset_failure") })
	if err := s.ResetPassword(pr, "new"); !errors.Is(err, failure) {
		t.Errorf("reset error: %v", err)
	}
	current, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.PasswordHash != "old" {
		t.Error("reset changed password despite failed revocation")
	}
	if _, err := s.GetPasswordResetByHash("reset"); err != nil {
		t.Errorf("reset consumption not rolled back: %v", err)
	}
}

func TestCredentialIssuanceRejectsStaleSnapshot(t *testing.T) {
	s := newTestStore(t)
	u := &models.User{Username: "alice", PasswordHash: "old", Email: "old@example.com"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserPassword(u.ID, "new", u.PasswordHash, ""); err != nil {
		t.Fatal(err)
	}
	pr := &models.PasswordReset{UserID: u.ID, TokenHash: "reset", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreatePasswordReset(pr, u); !errors.Is(err, ErrCredentialsChanged) {
		t.Errorf("stale reset issuance: %v", err)
	}
	c := &models.EmailConfirmation{UserID: u.ID, TokenHash: "confirm", Email: "attacker@example.com", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.StartEmailConfirmation(c, u.PasswordHash); !errors.Is(err, ErrCredentialsChanged) {
		t.Errorf("stale confirmation issuance: %v", err)
	}
	if err := s.UpdateUserPassword(u.ID, "attacker", u.PasswordHash, ""); !errors.Is(err, ErrCredentialsChanged) {
		t.Errorf("stale password change: %v", err)
	}
	current, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserEmail(u.ID, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePasswordReset(pr, current); !errors.Is(err, ErrCredentialsChanged) {
		t.Errorf("old email reset issuance: %v", err)
	}
}
