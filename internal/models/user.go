package models

import "time"

// User is a panel account. MVP: a single admin (multi-user/RBAC comes later).
type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"uniqueIndex;size:190" json:"username"`
	PasswordHash string    `json:"-"` // argon2id encoded string, never serialized
	IsAdmin      bool      `json:"isAdmin"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Session is a server-side bearer session (opaque random token).
type Session struct {
	Token     string    `gorm:"primaryKey;size:128" json:"-"`
	UserID    uint      `gorm:"index" json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
