package models

import "time"

// Admin permissions are coarse-grained capabilities over panel-wide
// administration. A superadmin (User.IsAdmin) implicitly holds every one of
// them; a scoped admin holds only those granted through their AdminRole. This
// lets you delegate, say, template or user management without handing out full
// control of the panel.
const (
	AdminPermServers       = "servers"        // act as admin on every server (view/power/files/delete/suspend any)
	AdminPermUsers         = "users"          // manage user accounts (not admin status, which stays superadmin-only)
	AdminPermTemplates     = "templates"      // import/edit/delete templates (eggs)
	AdminPermClusters      = "clusters"       // manage the cluster registry
	AdminPermDatabaseHosts = "database-hosts" // manage database hosts
	AdminPermNotifications = "notifications"  // manage global notification channels
	AdminPermSettings      = "settings"       // email/SMTP + backup configuration
	AdminPermAudit         = "audit"          // view the global activity log
)

// AllAdminPermissions is the full catalog a role can be granted.
var AllAdminPermissions = []string{
	AdminPermServers, AdminPermUsers, AdminPermTemplates, AdminPermClusters,
	AdminPermDatabaseHosts, AdminPermNotifications, AdminPermSettings, AdminPermAudit,
}

// ValidAdminPermission reports whether p is a known admin permission.
func ValidAdminPermission(p string) bool {
	for _, x := range AllAdminPermissions {
		if x == p {
			return true
		}
	}
	return false
}

// AdminRole is a named, reusable bundle of admin permissions that can be
// assigned to users to grant scoped administrative access.
type AdminRole struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Name        string    `gorm:"uniqueIndex;size:190" json:"name"`
	Description string    `json:"description"`
	Permissions []string  `gorm:"serializer:json" json:"permissions"`
}

// Has reports whether the role includes admin permission p.
func (r *AdminRole) Has(p string) bool {
	for _, x := range r.Permissions {
		if x == p {
			return true
		}
	}
	return false
}
