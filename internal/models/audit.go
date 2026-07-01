package models

import "time"

// AuditEntry records a mutating action for accountability. It is append-only.
type AuditEntry struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	UserID   uint   `gorm:"index" json:"userId"`
	Username string `json:"username"`
	// ServerID is 0 for panel-wide actions (e.g. user management).
	ServerID uint   `gorm:"index" json:"serverId,omitempty"`
	Action   string `json:"action"`
	Detail   string `json:"detail,omitempty"`

	// ServerName is the slug of the server this entry concerns, filled in at read
	// time for the global log so an admin can tell which server an action hit.
	// Not persisted; empty for panel-wide actions or deleted servers.
	ServerName string `gorm:"-" json:"serverName,omitempty"`
}
