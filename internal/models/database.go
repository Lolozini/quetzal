package models

import "time"

// Database host kinds.
const (
	// DBHostExternal is a pre-existing MySQL/MariaDB server the admin registers
	// with admin credentials; Quetzal only provisions databases/users on it.
	DBHostExternal = "external"
	// DBHostManaged is a MariaDB instance Quetzal deploys and owns in-cluster
	// (Deployment + PVC + Service + Secret), exposed as a host of the registry.
	DBHostManaged = "managed"
)

// DatabaseHost is a MySQL/MariaDB server Quetzal can provision databases on.
// Admin credentials are stored encrypted at rest and never returned by the API.
type DatabaseHost struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	Name string `json:"name"`
	Kind string `gorm:"size:16" json:"kind"` // external | managed

	// Host/Port the control plane uses to administer the server. For a managed
	// host this is the in-cluster Service DNS name.
	Host string `json:"host"`
	Port int    `json:"port"`

	// ConnectHost/ConnectPort is what is handed to game servers and shown to
	// users. Defaults to Host/Port; lets an admin advertise a different address
	// (e.g. a public name) than the one used to administer.
	ConnectHost string `json:"connectHost"`
	ConnectPort int    `json:"connectPort"`

	// Admin credentials used to CREATE DATABASE/USER. AdminPasswordEnc is
	// encrypted at rest and never serialized.
	AdminUser        string `json:"adminUser"`
	AdminPasswordEnc string `json:"-"`

	// MaxDatabases caps how many databases may live on this host (0 = unlimited).
	MaxDatabases int `json:"maxDatabases"`

	// Managed-host fields (Kind == managed): the controller reconciles a MariaDB
	// workload from these. ClusterID selects the target cluster.
	ClusterID   uint   `json:"clusterId,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Image       string `json:"image,omitempty"`
	StorageSize string `json:"storageSize,omitempty"`

	// Observed status, refreshed on test/connect.
	Reachable     bool       `json:"reachable"`
	StatusMessage string     `json:"statusMessage,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
}

// AdminAddr returns the host:port the control plane administers the server on.
func (h *DatabaseHost) AdminAddr() (string, int) {
	return h.Host, h.Port
}

// ClientHost/ClientPort return the address handed to servers/users (falls back
// to the admin address when no separate connect address is set).
func (h *DatabaseHost) ClientHost() string {
	if h.ConnectHost != "" {
		return h.ConnectHost
	}
	return h.Host
}

func (h *DatabaseHost) ClientPort() int {
	if h.ConnectPort != 0 {
		return h.ConnectPort
	}
	return h.Port
}

// ServerDatabase is a database (schema + scoped user) provisioned for a server
// on a DatabaseHost. The user password is stored encrypted (reversible, so it
// can be shown again to authorized users) and never serialized directly.
type ServerDatabase struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`

	ServerID uint `gorm:"index" json:"serverId"`
	HostID   uint `json:"hostId"`

	DatabaseName string `json:"databaseName"`
	Username     string `json:"username"`
	PasswordEnc  string `json:"-"`
	// Remote is the MySQL "host" part of the user (allowed-from pattern). "%" by
	// default (connect from anywhere with the credentials).
	Remote string `json:"remote"`
}
