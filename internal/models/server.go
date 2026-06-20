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

// ExposeType selects how a server's ports are made reachable. It maps directly
// to a Kubernetes Service type and is game-agnostic.
type ExposeType string

const (
	// ExposeClusterIP keeps the server reachable only inside the cluster (default).
	ExposeClusterIP ExposeType = "ClusterIP"
	// ExposeNodePort publishes each port on every node's IP at an allocated port.
	ExposeNodePort ExposeType = "NodePort"
	// ExposeLoadBalancer requests an external load balancer (MetalLB / cloud LB).
	ExposeLoadBalancer ExposeType = "LoadBalancer"
)

// Expose configures external reachability for a server's ports.
type Expose struct {
	Type ExposeType `json:"type,omitempty"`
	// Annotations are copied onto the Service. They stay provider-neutral: use
	// them for external-dns hostnames, MetalLB address pools, cloud LB hints,
	// etc. Nothing is hardcoded.
	Annotations map[string]string `json:"annotations,omitempty"`
	// PreserveClientIP sets externalTrafficPolicy: Local so the game server sees
	// the real player IP (matters for bans, geo, anti-abuse). Defaults to true
	// for NodePort/LoadBalancer when unset; ignored for ClusterIP.
	PreserveClientIP *bool `json:"preserveClientIP,omitempty"`
}

// ServiceType returns the effective exposure type, defaulting to ClusterIP.
func (e Expose) ServiceType() ExposeType {
	if e.Type == "" {
		return ExposeClusterIP
	}
	return e.Type
}

// External reports whether this exposure publishes the server outside the cluster.
func (e Expose) External() bool {
	t := e.ServiceType()
	return t == ExposeNodePort || t == ExposeLoadBalancer
}

// LocalTraffic reports whether externalTrafficPolicy should be Local.
func (e Expose) LocalTraffic() bool {
	if e.PreserveClientIP != nil {
		return *e.PreserveClientIP
	}
	return e.External()
}

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
	Phase Phase `json:"phase"`
	// Endpoints lists reachable addresses (external when exposed, otherwise the
	// in-cluster DNS names).
	Endpoints []string `json:"endpoints,omitempty"`
	// Address is the primary address players connect to (the primary port's
	// external endpoint when exposed).
	Address    string `json:"address,omitempty"`
	Message    string `json:"message,omitempty"`
	CrashCount int    `json:"crashCount,omitempty"`
	DiskUsed   int64  `json:"diskUsed,omitempty"` // bytes
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

	Resources Resources         `gorm:"serializer:json" json:"resources"`
	Env       map[string]string `gorm:"serializer:json" json:"env"`
	// SecretEnvEnc holds sensitive env values, encrypted at rest (never clear
	// text in the DB, never serialized to API clients).
	SecretEnvEnc string            `json:"-"`
	Storage      Storage           `gorm:"serializer:json" json:"storage"`
	Ports        []PortSpec        `gorm:"serializer:json" json:"ports,omitempty"`
	Expose       Expose            `gorm:"serializer:json" json:"expose"`
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
