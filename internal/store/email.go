package store

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/lolozini/quetzal/internal/models"
)

// Setting keys for system email / password-reset configuration.
const (
	// SettingSMTP holds the sealed SMTP config map (host, port, username,
	// password, from, tls) used for outbound system email (password reset).
	SettingSMTP = "smtp"
	// SettingPublicURL is the panel's external base URL, used to build absolute
	// links in emails. Configured explicitly (not derived from request headers)
	// so a spoofed Host can't poison reset links.
	SettingPublicURL = "public_url"
	// SettingRequire2FA is the panel-wide second-factor policy: who must hold
	// one before their session reaches anything beyond enrolment.
	SettingRequire2FA = "require_2fa"
)

// GetUserByEmail returns the user with the given email (case-insensitive), or
// ErrNotFound. Email is optional, so empty input never matches; nor does an
// address two accounts have, which addresses were not kept from before
// EmailTaken: the reset of one used to go to the oldest of them.
func (s *Store) GetUserByEmail(email string) (*models.User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, ErrNotFound
	}
	var us []models.User
	if err := s.db.Where("lower(email) = ?", email).Limit(2).Find(&us).Error; err != nil {
		return nil, err
	}
	if len(us) != 1 {
		return nil, ErrNotFound
	}
	return &us[0], nil
}

// EmailTaken reports whether an account other than except has the address,
// case aside. An address is one account's: two sharing one could not tell
// whose password a reset to it was for.
func (s *Store) EmailTaken(email string, except uint) (bool, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return false, nil
	}
	var n int64
	err := s.db.Model(&models.User{}).Where("lower(email) = ? AND id <> ?", email, except).Count(&n).Error
	return n > 0, err
}

// UpdateUserEmail sets a user's email (empty clears it), unconfirmed: what an
// administrator writes, or an account with no mail to confirm it by. Any
// address waiting for confirmation is dropped, and its links with it.
func (s *Store) UpdateUserEmail(id uint, email string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", id).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		return tx.Model(&models.User{ID: id}).Select("email", "email_verified", "pending_email").
			Updates(models.User{Email: strings.TrimSpace(email)}).Error
	})
}

// ---- email confirmation ----

// StartEmailConfirmation records a link sent to email, replacing any earlier
// one. An address other than the account's own becomes its pending address.
func (s *Store) StartEmailConfirmation(c *models.EmailConfirmation) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var u models.User
		if err := tx.First(&u, c.UserID).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", c.UserID).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		pending := c.Email
		if pending == u.Email {
			pending = ""
		}
		if err := tx.Model(&u).Select("pending_email").Updates(models.User{PendingEmail: pending}).Error; err != nil {
			return err
		}
		return tx.Create(c).Error
	})
}

// CancelPendingEmail drops the address waiting for confirmation, and its link.
func (s *Store) CancelPendingEmail(userID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		return tx.Model(&models.User{ID: userID}).Select("pending_email").Updates(models.User{}).Error
	})
}

// ErrConfirmationInvalid is a confirmation link that is unknown, used, expired,
// or for an address the account no longer asks for.
var ErrConfirmationInvalid = errors.New("invalid or expired confirmation link")

// ConfirmEmail makes the address a link was sent to the account's confirmed
// one. The link is used up. An address another account took meanwhile is
// refused with ErrDuplicate.
func (s *Store) ConfirmEmail(tokenHash string) (*models.User, error) {
	var u models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var c models.EmailConfirmation
		if err := tx.Where("token_hash = ?", tokenHash).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConfirmationInvalid
			}
			return err
		}
		if err := tx.Where("user_id = ?", c.UserID).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		if time.Now().After(c.ExpiresAt) {
			return ErrConfirmationInvalid
		}
		if err := tx.First(&u, c.UserID).Error; err != nil {
			return err
		}
		// The account asked for another address since, or cleared it.
		if c.Email != u.PendingEmail && c.Email != u.Email {
			return ErrConfirmationInvalid
		}
		var n int64
		if err := tx.Model(&models.User{}).Where("lower(email) = ? AND id <> ?", strings.ToLower(c.Email), u.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrDuplicate
		}
		u.Email, u.EmailVerified, u.PendingEmail = c.Email, true, ""
		return tx.Model(&u).Select("email", "email_verified", "pending_email").Updates(&u).Error
	})
	if errors.Is(err, ErrConfirmationInvalid) {
		// The link was used up all the same; commit that.
		if hErr := s.db.Where("token_hash = ?", tokenHash).Delete(&models.EmailConfirmation{}).Error; hErr != nil {
			return nil, hErr
		}
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ---- password reset tokens ----

// CreatePasswordReset stores a reset token (hash only).
func (s *Store) CreatePasswordReset(pr *models.PasswordReset) error {
	return s.db.Create(pr).Error
}

// GetPasswordResetByHash returns a reset by token hash, or ErrNotFound.
func (s *Store) GetPasswordResetByHash(hash string) (*models.PasswordReset, error) {
	var pr models.PasswordReset
	if err := s.db.Where("token_hash = ?", hash).First(&pr).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &pr, nil
}

// DeletePasswordResetsForUser removes all of a user's reset tokens (after a
// successful reset, or when issuing a fresh one).
func (s *Store) DeletePasswordResetsForUser(userID uint) error {
	return s.db.Where("user_id = ?", userID).Delete(&models.PasswordReset{}).Error
}

// The values SettingRequire2FA takes.
const (
	Require2FAOff    = "off"    // nobody is required to enrol
	Require2FAAdmins = "admins" // superadmins and scoped admins
	Require2FAAll    = "all"    // every account
)

// DeleteExpiredPasswordResets drops tokens past their expiry. Returns the count.
func (s *Store) DeleteExpiredPasswordResets() (int64, error) {
	res := s.db.Where("expires_at < ?", time.Now()).Delete(&models.PasswordReset{})
	return res.RowsAffected, res.Error
}

// DeleteSessionsForUser invalidates every session of a user (used after a
// password reset so existing logins can't continue).
func (s *Store) DeleteSessionsForUser(userID uint) error {
	return s.DeleteSessionsForUserExcept(userID, "")
}

// DeleteSessionsForUserExcept invalidates a user's sessions but keeps the one
// whose token hash is keepHash (empty keeps none). Changing a password is what
// someone does when they think a session has been stolen, so every other login
// has to end — while the client asking for the change stays signed in.
func (s *Store) DeleteSessionsForUserExcept(userID uint, keepHash string) error {
	q := s.db.Where("user_id = ?", userID)
	if keepHash != "" {
		q = q.Where("token <> ?", keepHash)
	}
	return q.Delete(&models.Session{}).Error
}

// ---- system SMTP settings ----

// GetSMTPConfig returns the sealed SMTP config map (empty if unconfigured).
func (s *Store) GetSMTPConfig() (map[string]string, error) {
	blob, err := s.GetSetting(SettingSMTP)
	if err != nil {
		return nil, err
	}
	return s.OpenSecrets(blob)
}

// SetSMTPConfig seals and stores the SMTP config map (nil/empty clears it).
func (s *Store) SetSMTPConfig(cfg map[string]string) error {
	blob, err := s.SealSecrets(cfg)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingSMTP, blob)
}
