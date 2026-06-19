package models

import "time"

// DesiredState is the power state requested by the user.
type DesiredState string

const (
	StateRunning   DesiredState = "Running"
	StateStopped   DesiredState = "Stopped"
	StateSuspended DesiredState = "Suspended" // admin-enforced stop
)

// Phase is the observed lifecycle phase, written by the controller.
type Phase string

const (
	PhaseInstalling Phase = "Installing"
	PhaseStarting   Phase = "Starting"
	PhaseRunning    Phase = "Running"
	PhaseStopping   Phase = "Stopping"
	PhaseStopped    Phase = "Stopped"
	PhaseCrashed    Phase = "Crashed"
	PhaseSuspended  Phase = "Suspended"
	PhaseError      Phase = "Error"
)

// StorageType selects how persistent data is backed.
type StorageType string

const (
	StoragePVC      StorageType = "pvc"      // dynamic PVC via storageClass
	StorageHostPath StorageType = "hostPath" // direct node path (homelab)
)

// Storage describes a server's persistent data backing.
type Storage struct {
	Type           StorageType `json:"type"`
	Size           string      `json:"size,omitempty"`         // e.g. "20Gi" (pvc)
	StorageClass   string      `json:"storageClass,omitempty"` // empty = cluster default
	HostPath       string      `json:"hostPath,omitempty"`     // for hostPath
	RetainOnDelete bool        `json:"retainOnDelete,omitempty"`
}

// Resources holds the container resource limits/requests.
type Resources struct {
	Memory string `json:"memory,omitempty"` // e.g. "8Gi"
	CPU    string `json:"cpu,omitempty"`    // e.g. "4"
}

// Status is the controller-written observed state.
type Status struct {
	Phase      Phase    `json:"phase"`
	Endpoints  []string `json:"endpoints,omitempty"`
	Message    string   `json:"message,omitempty"`
	CrashCount int      `json:"crashCount,omitempty"`
	DiskUsed   int64    `json:"diskUsed,omitempty"` // bytes
}

// Server is a deployable game server instance. The database row is the source
// of truth; the controller projects it into native Kubernetes objects.
type Server struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Slug      string    `gorm:"uniqueIndex;size:190" json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	DisplayName string `json:"displayName"`
	OwnerID     uint   `json:"ownerId,omitempty"`

	TemplateID      uint `gorm:"index" json:"templateId"`
	TemplateVersion int  `json:"templateVersion"`
	// Image is the selected template image ref.
	Image string `json:"image"`

	// Namespace is the per-server Kubernetes namespace the controller manages.
	Namespace string `gorm:"size:253" json:"namespace"`

	DesiredState DesiredState `gorm:"default:Stopped" json:"desiredState"`

	Resources    Resources         `gorm:"serializer:json" json:"resources"`
	Env          map[string]string `gorm:"serializer:json" json:"env"`
	Storage      Storage           `gorm:"serializer:json" json:"storage"`
	Ports        []PortSpec        `gorm:"serializer:json" json:"ports,omitempty"`
	NodeSelector map[string]string `gorm:"serializer:json" json:"nodeSelector,omitempty"`

	Status Status `gorm:"serializer:json" json:"status"`
}

// Replicas returns the desired pod replica count for the current power state.
func (s *Server) Replicas() int32 {
	if s.DesiredState == StateRunning {
		return 1
	}
	return 0
}
