package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/lolozini/quetzal/internal/models"
)

// RateAllow records an attempt against key and reports whether it is within
// limit for the current window, plus when that window ends.
//
// The read and the write are one locked transaction, so two apiserver replicas
// counting the same key cannot both see "one below the limit" and both allow.
func (s *Store) RateAllow(key string, limit int, window time.Duration, now time.Time) (bool, time.Time, error) {
	var (
		allowed bool
		resetAt time.Time
	)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var c models.RateCounter
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", key).First(&c).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c = models.RateCounter{Key: key, Count: 1, ResetAt: now.Add(window)}
			allowed, resetAt = true, c.ResetAt
			return tx.Create(&c).Error
		case err != nil:
			return err
		}
		if now.After(c.ResetAt) { // the window rolled over
			c.Count, c.ResetAt = 1, now.Add(window)
			allowed, resetAt = true, c.ResetAt
			return tx.Save(&c).Error
		}
		resetAt = c.ResetAt
		if c.Count >= limit {
			allowed = false
			return nil // nothing to write: a refused attempt does not extend the window
		}
		c.Count++
		allowed = true
		return tx.Save(&c).Error
	})
	return allowed, resetAt, err
}

// RateReset clears a key, e.g. after a successful login so earlier failures
// stop counting against the account.
func (s *Store) RateReset(key string) error {
	return s.db.Where("key = ?", key).Delete(&models.RateCounter{}).Error
}

// RateGC drops windows that have already rolled over.
func (s *Store) RateGC(now time.Time) error {
	return s.db.Where("reset_at < ?", now).Delete(&models.RateCounter{}).Error
}
