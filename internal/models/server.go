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
	PhaseHibernated Phase = "Hibernated"
	PhaseError      Phase = "Error"
)

// Hibernation configures automatic scale-to-zero of an idle server (no active
// player connections). Waking is manual (start); wake-on-connect is a future
// shared-proxy enhancement.
type Hibernation struct {
	Enabled     bool `json:"enabled"`
	IdleMinutes int  `json:"idleMinutes"` // 0 falls back to a default
	// WakeOnConnect deploys a tiny activator while hibernated that listens on the
	// server's TCP ports and wakes it when a client connects, then drops the
	// connection (clients reconnect). Out of the data path when awake. TCP only.
	WakeOnConnect bool `json:"wakeOnConnect"`
	// Proxy uses an always-in-path TCP+UDP proxy instead: it wakes the server on
	// a new flow and forwards traffic transparently (no reconnect), supports UDP,
	// and reports activity so UDP servers can auto-hibernate too. Trade-offs: a
	// small extra hop and the server sees the proxy's IP, not the client's.
	// When set, it supersedes WakeOnConnect.
	Proxy bool `json:"proxy"`
}

// WakesOnConnect reports whether a connection should wake this server, in either
// activator mode (drop or proxy).
func (h Hibernation) WakesOnConnect() bool {
	return h.Enabled && (h.WakeOnConnect || h.Proxy)
}

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

// SFTPConfig configures opt-in SFTP access to a server's data volume. When
// enabled, the controller runs a key-only SFTP sidecar (in the game image, so
// files are owned by the server's user) and exposes it on a NodePort.
type SFTPConfig struct {
	Enabled bool `json:"enabled"`
}

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
	StoragePVC StorageType = "pvc" // dynamic PVC via storageClass
)

// Storage describes a server's persistent data backing. Data is always a PVC:
// hostPath was removed because it lets a tenant mount arbitrary node paths (a
// host-escape vector for the untrusted code game pods run, and forbidden by the
// baseline/restricted Pod Security Standards) and has no scheduling affinity,
// which breaks rescheduling and cross-cluster transfer. Single-node setups use a
// local provisioner (e.g. local-path) as the storageClass.
type Storage struct {
	Type         StorageType `json:"type"`
	Size         string      `json:"size,omitempty"`         // e.g. "20Gi"
	StorageClass string      `json:"storageClass,omitempty"` // empty = cluster default
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

	// ClusterID is the target cluster (from the cluster registry). 0 / the
	// in-cluster cluster means the control plane's own cluster.
	ClusterID uint `gorm:"index" json:"clusterId"`

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

	// SFTP, when enabled, adds a key-only SFTP sidecar exposing the data volume.
	SFTP SFTPConfig `gorm:"serializer:json" json:"sftp"`

	// EULAAccepted records the user's acceptance of the Minecraft EULA for
	// templates that declare the "eula" egg feature. When true, the controller
	// renders eula.txt=true into the data volume at startup; when false it writes
	// nothing, so the server stops asking for acceptance (matching Pterodactyl's
	// eula feature, where the daemon writes the file once the user accepts).
	EULAAccepted bool `json:"eulaAccepted"`

	// InstallGeneration drives reinstall: the install init container writes it to
	// the install marker and re-runs the install script whenever the marker's
	// generation differs (so bumping it triggers a reinstall on the next start).
	// New servers start at 1; a 0/legacy marker is treated as installed.
	InstallGeneration int `json:"installGeneration"`
	// InstallWipe, when set, makes the next install run wipe the data volume
	// first (reinstall-from-scratch). It only takes effect when the install
	// actually re-runs (generation mismatch).
	InstallWipe bool `json:"-"`

	// Hibernation policy and system-managed state.
	Hibernation Hibernation `gorm:"serializer:json" json:"hibernation"`
	// Hibernated is set by the controller when an idle server is scaled to zero.
	Hibernated   bool       `json:"hibernated"`
	LastActiveAt *time.Time `json:"lastActiveAt,omitempty"`

	// Transfer is the in-progress migration to another cluster (nil when none).
	// Driven by the controller's transfer manager.
	Transfer *TransferState `gorm:"serializer:json" json:"transfer,omitempty"`

	Status Status `gorm:"serializer:json" json:"status"`
}

// Replicas returns the desired pod replica count. A server runs only when the
// user wants it Running and it isn't hibernated (scaled to zero on idle).
func (s *Server) Replicas() int32 {
	if s.DesiredState == StateRunning && !s.Hibernated {
		return 1
	}
	return 0
}
