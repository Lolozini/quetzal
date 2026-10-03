package store

import (
	"crypto/rand"
	"crypto/subtle"
	"strings"

	"gorm.io/gorm/clause"

	"github.com/lolozini/quetzal/internal/models"
)

// settingSetupCode holds the code the first-run setup asks for.
const settingSetupCode = "setup_code"

// setupAlphabet leaves out the characters read one for another: 0 and O, 1, I
// and L.
const setupAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// SetupCode returns the code the first-run setup asks for, made once and kept
// in the database, so every replica, and every restart, prints the same one.
// It is "" once an account exists: there is no setup left to protect.
func (s *Store) SetupCode() (string, error) {
	if n, err := s.CountUsers(); err != nil || n > 0 {
		return "", err
	}
	if code, err := s.GetSetting(settingSetupCode); err != nil || code != "" {
		return code, err
	}
	code, err := newSetupCode()
	if err != nil {
		return "", err
	}
	// Two replicas starting together: whichever writes first, both read back
	// the same code.
	if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&models.Setting{Key: settingSetupCode, Value: code}).Error; err != nil {
		return "", err
	}
	return s.GetSetting(settingSetupCode)
}

// ClearSetupCode drops the setup code once the setup is done.
func (s *Store) ClearSetupCode() error {
	return s.db.Delete(&models.Setting{}, "key = ?", settingSetupCode).Error
}

// newSetupCode is twelve characters of setupAlphabet in three groups,
// XXXX-XXXX-XXXX: nearly sixty bits.
func newSetupCode() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(setupAlphabet[int(c)%len(setupAlphabet)])
	}
	return sb.String(), nil
}

// SameSetupCode compares a code as typed with the real one: case, spaces and
// dashes aside.
func SameSetupCode(typed, code string) bool {
	norm := func(s string) string {
		return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	}
	return code != "" && subtle.ConstantTimeCompare([]byte(norm(typed)), []byte(norm(code))) == 1
}
