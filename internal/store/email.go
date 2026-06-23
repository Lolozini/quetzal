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
)

// GetUserByEmail returns the user with the given email (case-insensitive), or
// ErrNotFound. Email is optional, so empty input never matches.
func (s *Store) GetUserByEmail(email string) (*models.User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, ErrNotFound
	}
	var u models.User
	if err := s.db.Where("lower(email) = ?", email).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// UpdateUserEmail sets a user's email (empty clears it).
func (s *Store) UpdateUserEmail(id uint, email string) error {
	return s.db.Model(&models.User{ID: id}).Select("email").
		Updates(models.User{Email: strings.TrimSpace(email)}).Error
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

// DeleteExpiredPasswordResets drops tokens past their expiry. Returns the count.
func (s *Store) DeleteExpiredPasswordResets() (int64, error) {
	res := s.db.Where("expires_at < ?", time.Now()).Delete(&models.PasswordReset{})
	return res.RowsAffected, res.Error
}

// DeleteSessionsForUser invalidates every session of a user (used after a
// password reset so existing logins can't continue).
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
