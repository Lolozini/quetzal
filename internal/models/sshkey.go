package models

import "time"

// SSHKey is a user's registered SSH public key, used to authenticate SFTP
// access to servers they can manage files on. Only the public key is stored.
type SSHKey struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	UserID uint   `gorm:"index;uniqueIndex:idx_ssh_keys_user_fingerprint" json:"userId"`
	Name   string `json:"name"`
	// PublicKey is the authorized_keys line (e.g. "ssh-ed25519 AAAA...").
	PublicKey string `json:"publicKey"`
	// Fingerprint is the SHA256 fingerprint. A user holds a key once: with two
	// rows for one key, deleting either left the key working through the other.
	Fingerprint string `gorm:"uniqueIndex:idx_ssh_keys_user_fingerprint" json:"fingerprint"`
}

// SSHKeyUniqueIndex is the index that keeps a user from holding a key twice.
const SSHKeyUniqueIndex = "idx_ssh_keys_user_fingerprint"
