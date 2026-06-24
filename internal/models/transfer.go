package models

import "time"

// TransferPhase tracks a server's migration between clusters. Data moves via the
// backup target (restic → S3, cluster-agnostic): back up on the source, flip the
// cluster, restore on the destination, then delete the source namespace.
type TransferPhase string

const (
	// TransferBackingUp: the server is stopped and a backup of its data is taken
	// on the source cluster (gated on the source pod being gone, for a quiescent
	// snapshot).
	TransferBackingUp TransferPhase = "BackingUp"
	// TransferRestoring: the server now belongs to the target cluster; once its
	// (empty) volume exists there, the snapshot is restored into it.
	TransferRestoring TransferPhase = "Restoring"
)

// TransferState is the in-progress migration of a server to another cluster. It
// is nil when no transfer is running.
type TransferState struct {
	Phase         TransferPhase `json:"phase"`
	SourceCluster uint          `json:"sourceCluster"`
	TargetCluster uint          `json:"targetCluster"`
	// BackupID is the source snapshot; RestoreID is the destination restore op.
	BackupID  uint `json:"backupId,omitempty"`
	RestoreID uint `json:"restoreId,omitempty"`
	// PrevState is the power state to restore once the transfer completes.
	PrevState DesiredState `json:"prevState"`
	StartedAt time.Time    `json:"startedAt"`
	Message   string       `json:"message,omitempty"`
}
