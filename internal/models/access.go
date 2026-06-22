package models

import "time"

// Server permissions granted to a subuser. The owner and admins implicitly hold
// all of them.
const (
	PermView      = "view"      // see the server and its status
	PermPower     = "power"     // start/stop/restart/kill
	PermConsole   = "console"   // live console
	PermSchedules = "schedules" // manage scheduled tasks
	PermBackups   = "backups"   // create/restore/delete backups
	PermFiles     = "files"     // browse/edit the server's files
	PermSettings  = "settings"  // edit exposure / resources
	PermDelete    = "delete"    // delete the server
)

// AllPermissions is the full set a subuser can be granted.
var AllPermissions = []string{
	PermView, PermPower, PermConsole, PermSchedules, PermBackups, PermFiles, PermSettings, PermDelete,
}

// ValidPermission reports whether p is a known permission.
func ValidPermission(p string) bool {
	for _, x := range AllPermissions {
		if x == p {
			return true
		}
	}
	return false
}

// ServerAccess grants a user (subuser) a scoped set of permissions on a server.
type ServerAccess struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	ServerID    uint     `gorm:"uniqueIndex:idx_access_server_user" json:"serverId"`
	UserID      uint     `gorm:"uniqueIndex:idx_access_server_user" json:"userId"`
	Permissions []string `gorm:"serializer:json" json:"permissions"`

	// Username is populated for API responses (not stored).
	Username string `gorm:"-" json:"username,omitempty"`
}

// Has reports whether the grant includes permission p.
func (a *ServerAccess) Has(p string) bool {
	for _, x := range a.Permissions {
		if x == p {
			return true
		}
	}
	return false
}
