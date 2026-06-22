package store

import (
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/totp"
)

// SetUserTOTPSecret stores a pending (not-yet-confirmed) TOTP secret, encrypted.
// Enrollment is confirmed separately via EnableUserTOTP.
func (s *Store) SetUserTOTPSecret(id uint, secret string) error {
	enc, err := s.sealValue(secret)
	if err != nil {
		return err
	}
	return s.db.Model(&models.User{}).Where("id = ?", id).
		Updates(map[string]any{"totp_secret_enc": enc, "totp_enabled": false}).Error
}

// UserTOTPSecret returns the decrypted TOTP secret for a user.
func (s *Store) UserTOTPSecret(u *models.User) (string, error) {
	return s.openValue(u.TOTPSecretEnc)
}

// EnableUserTOTP confirms enrollment: marks 2FA active and stores the recovery
// code hashes (replacing any previous set).
func (s *Store) EnableUserTOTP(id uint, recoveryHashes []string) error {
	return s.db.Model(&models.User{ID: id}).
		Select("totp_enabled", "recovery_codes").
		Updates(models.User{TOTPEnabled: true, RecoveryCodes: recoveryHashes}).Error
}

// DisableUserTOTP clears all two-factor material for a user.
func (s *Store) DisableUserTOTP(id uint) error {
	return s.db.Model(&models.User{ID: id}).
		Select("totp_secret_enc", "totp_enabled", "recovery_codes").
		Updates(models.User{TOTPSecretEnc: "", TOTPEnabled: false, RecoveryCodes: nil}).Error
}

// ConsumeRecoveryCode atomically checks code against the user's unused recovery
// codes and, on a match, removes it so it cannot be reused. Reports whether a
// code was consumed.
func (s *Store) ConsumeRecoveryCode(id uint, code string) (bool, error) {
	want := totp.HashRecovery(code)
	var u models.User
	if err := s.db.First(&u, id).Error; err != nil {
		return false, err
	}
	remaining := make([]string, 0, len(u.RecoveryCodes))
	found := false
	for _, h := range u.RecoveryCodes {
		if !found && h == want {
			found = true
			continue
		}
		remaining = append(remaining, h)
	}
	if !found {
		return false, nil
	}
	err := s.db.Model(&models.User{ID: id}).
		Select("recovery_codes").
		Updates(models.User{RecoveryCodes: remaining}).Error
	return err == nil, err
}
