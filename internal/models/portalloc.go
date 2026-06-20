package models

// PortAllocation records a node port reserved for a server from the control
// plane's port pool. Persisting allocations keeps a server's node port stable
// across reconciles (so firewall rules stay valid) and conflict-free across
// servers, rather than letting Kubernetes pick a fresh random port each time.
type PortAllocation struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// NodePort is the reserved port (unique across all servers).
	NodePort int32 `gorm:"uniqueIndex" json:"nodePort"`
	// ServerID owns the allocation; freed when the server is deleted.
	ServerID uint `gorm:"index" json:"serverId"`
	// PortName ties the allocation to a named port on the server.
	PortName string `json:"portName"`
}
