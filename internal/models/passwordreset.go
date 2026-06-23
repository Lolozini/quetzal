package models

import "time"

// PasswordReset is a single-use, time-limited token for self-service password
// reset. Only the SHA-256 hash of the token is stored (the clear token lives
// only in the email link), so a database leak does not yield usable tokens.
type PasswordReset struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	UserID    uint      `gorm:"index" json:"-"`
	TokenHash string    `gorm:"uniqueIndex;size:64" json:"-"`
	ExpiresAt time.Time `json:"-"`
	CreatedAt time.Time `json:"-"`
}
