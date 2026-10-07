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
		u, err := lockUser(tx, id)
		if err != nil {
			return err
		}
		if u.Email != strings.TrimSpace(email) {
			if err := tx.Where("user_id = ?", id).Delete(&models.PasswordReset{}).Error; err != nil {
				return err
			}
		}
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
func (s *Store) StartEmailConfirmation(c *models.EmailConfirmation, verifiedHash string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		u, err := lockUser(tx, c.UserID)
		if err != nil {
			return err
		}
		if u.PasswordHash != verifiedHash {
			return ErrCredentialsChanged
		}
		if err := tx.Where("user_id = ?", c.UserID).Delete(&models.EmailConfirmation{}).Error; err != nil {
			return err
		}
		pending := c.Email
		if pending == u.Email {
			pending = ""
		}
		if err := tx.Model(u).Select("pending_email").Updates(models.User{PendingEmail: pending}).Error; err != nil {
			return err
		}
		return tx.Create(c).Error
	})
}

// CancelPendingEmail drops the address waiting for confirmation, and its link.
func (s *Store) CancelPendingEmail(userID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if _, err := lockUser(tx, userID); err != nil {
			return err
		}
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
	var c models.EmailConfirmation
	if err := s.db.Where("token_hash = ?", tokenHash).First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrConfirmationInvalid
		}
		return nil, err
	}
	var u *models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		u, err = lockUser(tx, c.UserID)
		if err != nil {
			return err
		}
		// Recheck consumption after locking the account: a password change or
		// another confirmation may have invalidated our earlier snapshot.
		res := tx.Where("id = ? AND token_hash = ? AND expires_at > ?", c.ID, tokenHash, time.Now()).Delete(&models.EmailConfirmation{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 || (c.Email != u.PendingEmail && c.Email != u.Email) {
			return ErrConfirmationInvalid
		}
		var n int64
		if err := tx.Model(&models.User{}).Where("lower(email) = ? AND id <> ?", strings.ToLower(c.Email), u.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrDuplicate
		}
		if u.Email != c.Email {
			if err := tx.Where("user_id = ?", u.ID).Delete(&models.PasswordReset{}).Error; err != nil {
				return err
			}
		}
		u.Email, u.EmailVerified, u.PendingEmail = c.Email, true, ""
		return tx.Model(u).Select("email", "email_verified", "pending_email").Updates(u).Error
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ---- password reset tokens ----

// CreatePasswordReset replaces the active reset only if the account snapshot
// still matches: a link addressed to an old email or password state is refused.
func (s *Store) CreatePasswordReset(pr *models.PasswordReset, proof *models.User) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		u, err := lockUser(tx, pr.UserID)
		if err != nil {
			return err
		}
		if proof.ID != u.ID || proof.PasswordHash != u.PasswordHash || proof.Email != u.Email {
			return ErrCredentialsChanged
		}
		if err := tx.Where("user_id = ?", pr.UserID).Delete(&models.PasswordReset{}).Error; err != nil {
			return err
		}
		return tx.Create(pr).Error
	})
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

// DeleteSessionsForUser invalidates every session of a user.
// Password mutations use changePassword so revocation cannot fail separately.
func (s *Store) DeleteSessionsForUser(userID uint) error {
	return s.db.Where("user_id = ?", userID).Delete(&models.Session{}).Error
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
