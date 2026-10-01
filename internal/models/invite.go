package models

import "time"

// ServerInvite offers access to a server to whoever reads a given mailbox. The
// owner names an email address, not an account: the link mailed there is the
// invitation, and it is accepted from an account the reader signs in to, or one
// they create from it. Matching the address against accounts instead would
// hand the access to anyone who typed that address into their profile, since
// an account's email is never verified.
//
// Only the SHA-256 hash of the token is stored, like a password reset's.
type ServerInvite struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	ServerID    uint     `gorm:"uniqueIndex:idx_invite_server_email" json:"serverId"`
	Email       string   `gorm:"uniqueIndex:idx_invite_server_email;size:254" json:"email"`
	Permissions []string `gorm:"serializer:json" json:"permissions"`
	// InvitedBy is the account that sent it; its name is in the mail and on the
	// page the link opens.
	InvitedBy uint      `json:"invitedBy"`
	TokenHash string    `gorm:"uniqueIndex;size:64" json:"-"`
	ExpiresAt time.Time `json:"expiresAt"`

	// InvitedByName is filled for API responses (not stored).
	InvitedByName string `gorm:"-" json:"invitedByName,omitempty"`
}
