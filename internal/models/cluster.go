package models

import "time"

// Cluster is a Kubernetes cluster Quetzal can deploy servers to. The local
// cluster (InCluster) is the one the control plane itself runs in / is
// configured for and stores no credentials; remote clusters are reached via a
// stored kubeconfig, encrypted at rest and never returned by the API.
type Cluster struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	Slug string `gorm:"uniqueIndex;size:190" json:"slug"`
	Name string `json:"name"`

	// InCluster marks the control plane's own cluster: it uses the process's
	// in-cluster / default kubeconfig, so no credentials are stored.
	InCluster bool `json:"inCluster"`

	// KubeconfigEnc holds a remote cluster's kubeconfig, encrypted at rest.
	// Never serialized to API clients.
	KubeconfigEnc string `json:"-"`

	// DefaultStorageClass is the storageClass new servers on this cluster use for
	// their data PVC. Empty means the cluster's own default storageClass. It is
	// admin-controlled (chosen from the cluster's actual storageClasses), not set
	// per server by tenants.
	DefaultStorageClass string `json:"defaultStorageClass,omitempty"`

	// Observed status, refreshed on demand and periodically by the controller.
	Reachable     bool       `json:"reachable"`
	Version       string     `json:"version,omitempty"`
	NodeCount     int        `json:"nodeCount,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	StatusMessage string     `json:"statusMessage,omitempty"`
}
