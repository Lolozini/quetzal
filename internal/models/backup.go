package models

import "time"

// BackupConfig is the single-row, panel-wide backup target. It is provider
// neutral: any S3-compatible endpoint works (MinIO, OVH, AWS, Backblaze…),
// nothing is hardcoded. Secrets are stored encrypted at rest and never returned
// by the API.
type BackupConfig struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UpdatedAt time.Time `json:"updatedAt"`

	Endpoint string `json:"endpoint"` // host[:port], no scheme
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"` // optional path inside the bucket
	Region   string `json:"region,omitempty"`
	UseSSL   bool   `json:"useSSL"`

	// Encrypted secrets (envelope-encrypted via the store key); never serialized.
	AccessKeyEnc    string `json:"-"`
	SecretKeyEnc    string `json:"-"`
	RepoPasswordEnc string `json:"-"` // restic repository password (encryption key)

	// KeepLast is how many snapshots to retain per server (restic forget).
	KeepLast int `json:"keepLast"`
	// RunnerImage is the backup runner container image (restic).
	RunnerImage string `json:"runnerImage"`
}

// BackupDirection distinguishes a backup from a restore operation.
type BackupDirection string

const (
	DirBackup  BackupDirection = "backup"
	DirRestore BackupDirection = "restore"
)

// BackupPhase is the lifecycle of a backup/restore operation.
type BackupPhase string

const (
	BackupPending   BackupPhase = "Pending"
	BackupRunning   BackupPhase = "Running"
	BackupSucceeded BackupPhase = "Succeeded"
	BackupFailed    BackupPhase = "Failed"
)

// Backup records one backup or restore operation for a server. It maps to a
// restic snapshot (tagged with the backup ID) and is driven to completion by the
// controller via a one-shot Job.
type Backup struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`

	ServerID  uint            `gorm:"index" json:"serverId"`
	Direction BackupDirection `json:"direction"`
	Phase     BackupPhase     `json:"phase"`
	// SourceID, for a restore, is the backup being restored from.
	SourceID  uint   `json:"sourceId,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Message   string `json:"message,omitempty"`
	JobName   string `json:"jobName,omitempty"`
}
