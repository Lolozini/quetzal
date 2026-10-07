package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/lolozini/quetzal/internal/models"
)

// ErrCredentialsChanged means the password proof belongs to an older account state.
var ErrCredentialsChanged = errors.New("credentials changed; authenticate again")

// lockUser serializes credential changes and issuance across API replicas.
// SQLite takes its write lock at transaction start; Postgres locks this row.
func lockUser(tx *gorm.DB, id uint) (*models.User, error) {
	var u models.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// changePassword runs with the account locked. Any failed revocation rolls back
// the password as well, so success never leaves old recovery links live.
func changePassword(tx *gorm.DB, id uint, hash, keepSession string) error {
	if err := tx.Model(&models.User{}).Where("id = ?", id).
		Updates(map[string]any{"password_hash": hash, "pending_email": ""}).Error; err != nil {
		return err
	}
	if err := tx.Where("user_id = ?", id).Delete(&models.PasswordReset{}).Error; err != nil {
		return err
	}
	if err := tx.Where("user_id = ?", id).Delete(&models.EmailConfirmation{}).Error; err != nil {
		return err
	}
	q := tx.Where("user_id = ?", id)
	if keepSession != "" {
		q = q.Where("token <> ?", keepSession)
	}
	return q.Delete(&models.Session{}).Error
}

// ResetPassword consumes a still-valid reset and changes credentials in one
// transaction. A previously read token is not itself proof of current validity.
func (s *Store) ResetPassword(pr *models.PasswordReset, hash string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if _, err := lockUser(tx, pr.UserID); err != nil {
			return err
		}
		res := tx.Where("id = ? AND user_id = ? AND token_hash = ? AND expires_at > ?", pr.ID, pr.UserID, pr.TokenHash, time.Now()).Delete(&models.PasswordReset{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrNotFound
		}
		return changePassword(tx, pr.UserID, hash, "")
	})
}
