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

// EmailConfirmation is a link sent to an address, which proves its reader
// holds it: the account's pending address, or its current one when that was
// never confirmed. Like a reset token, only the token's hash is stored.
type EmailConfirmation struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"index"`
	Email     string `gorm:"size:190"`
	TokenHash string `gorm:"uniqueIndex;size:64"`
	ExpiresAt time.Time
	CreatedAt time.Time
}
