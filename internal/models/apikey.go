package models

import "time"

// APIKey is a long-lived bearer credential for the public API. Only a hash of
// the secret is stored; the plaintext is shown once at creation. A key inherits
// its owner's permissions.
type APIKey struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	UserID uint   `gorm:"index" json:"userId"`
	Name   string `json:"name"`
	// Prefix is the first chars of the token, shown for identification.
	Prefix string `json:"prefix"`
	// Hash is the SHA-256 (hex) of the full token; never serialized.
	Hash       string     `gorm:"uniqueIndex;size:64" json:"-"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}
