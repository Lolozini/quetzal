package models

import "time"

// User is a panel account. Admins have unrestricted access; regular users own
// their servers and may be granted scoped access to others as subusers.
type User struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	Username     string `gorm:"uniqueIndex;size:190" json:"username"`
	PasswordHash string `json:"-"` // argon2id encoded string, never serialized
	// Email is optional and used for self-service password reset. It is not
	// verified (no confirmation flow); login is always by username.
	Email     string    `gorm:"index;size:190" json:"email,omitempty"`
	IsAdmin   bool      `json:"isAdmin"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Per-user quotas (0 = unlimited), enforced at server creation. Admins are
	// exempt.
	MaxServers  int   `json:"maxServers"`
	MaxMemoryMB int64 `json:"maxMemoryMB"`
	MaxCPUMilli int64 `json:"maxCpuMilli"`

	// Two-factor authentication (opt-in TOTP). TOTPSecretEnc holds the encrypted
	// base32 secret (reversible: needed to compute codes). TOTPEnabled is set
	// once the user confirms enrollment with a valid code. RecoveryCodes are
	// SHA-256 hashes of the still-unused single-use codes. None are serialized.
	TOTPSecretEnc string   `json:"-"`
	TOTPEnabled   bool     `json:"twoFactorEnabled"`
	RecoveryCodes []string `gorm:"serializer:json" json:"-"`
}

// Session is a server-side bearer session (opaque random token).
type Session struct {
	Token     string    `gorm:"primaryKey;size:128" json:"-"`
	UserID    uint      `gorm:"index" json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
