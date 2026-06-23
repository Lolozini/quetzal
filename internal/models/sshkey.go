package models

import "time"

// SSHKey is a user's registered SSH public key, used to authenticate SFTP
// access to servers they can manage files on. Only the public key is stored.
type SSHKey struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	UserID uint   `gorm:"index" json:"userId"`
	Name   string `json:"name"`
	// PublicKey is the authorized_keys line (e.g. "ssh-ed25519 AAAA...").
	PublicKey string `json:"publicKey"`
	// Fingerprint is the SHA256 fingerprint, for display.
	Fingerprint string `json:"fingerprint"`
}
