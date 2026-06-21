package models

import "time"

// User is a panel account. Admins have unrestricted access; regular users own
// their servers and may be granted scoped access to others as subusers.
type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:190" json:"username"`
	PasswordHash string    `json:"-"` // argon2id encoded string, never serialized
	IsAdmin      bool      `json:"isAdmin"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`

	// Per-user quotas (0 = unlimited), enforced at server creation. Admins are
	// exempt.
	MaxServers  int   `json:"maxServers"`
	MaxMemoryMB int64 `json:"maxMemoryMB"`
	MaxCPUMilli int64 `json:"maxCpuMilli"`
}

// Session is a server-side bearer session (opaque random token).
type Session struct {
	Token     string    `gorm:"primaryKey;size:128" json:"-"`
	UserID    uint      `gorm:"index" json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
