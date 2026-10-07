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

// BackupDirection distinguishes a backup from a restore operation, and both
// from a database import.
type BackupDirection string

const (
	DirBackup  BackupDirection = "backup"
	DirRestore BackupDirection = "restore"
	// DirDatabaseImport loads an SQL file from the server's files into one of
	// its databases. It is driven like a restore -- a Job, on a stopped server
	// that cannot start until it is done -- but it lives with the databases,
	// not in the server's list of backups.
	DirDatabaseImport BackupDirection = "db-import"
)

// BackupPhase is the lifecycle of a backup/restore operation.
type BackupPhase string

const (
	BackupPending   BackupPhase = "Pending"
	BackupRunning   BackupPhase = "Running"
	BackupSucceeded BackupPhase = "Succeeded"
	BackupFailed    BackupPhase = "Failed"
	// BackupDeleting means the record is on its way out and its restic snapshot
	// is being forgotten from the repository. The row survives until the forget
	// Job succeeds, so a failure surfaces instead of silently leaving the data in
	// the bucket; on success the row is removed.
	BackupDeleting BackupPhase = "Deleting"
)

// Backup records one operation on a server's data -- a backup, a restore, or a
// database import -- driven to completion by the controller via a one-shot Job.
// A backup maps to a restic snapshot (tagged with the backup ID).
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
	// JobSpec is the immutable submission intent. A lost Create response can
	// be retried under the same name until JobUID records an observed Job.
	JobSpec string `gorm:"type:text" json:"-"`
	JobUID  string `json:"-"`
	// ClusterID binds the operation to its execution cluster; nil is legacy.
	ClusterID *uint `json:"-"`
	// Forgotten is a durable deletion result awaiting Job/record cleanup.
	// The record stays Deleting and must never be offered as a recovery point.
	Forgotten bool `json:"-"`
	// Target fingerprints the backup target its snapshot went to (see
	// backup.TargetID), so that changing the target does not leave backups
	// listed as restorable from a repository that does not hold them. Empty
	// for a backup made before targets were recorded and not yet stamped.
	Target string `gorm:"size:32" json:"-"`
	// OtherTarget is set in API answers for a backup made to a target the
	// panel no longer uses: it cannot be restored from the current one.
	OtherTarget bool `gorm:"-" json:"otherTarget,omitempty"`

	// Databases names, for a backup, the server's databases dumped into its
	// snapshot alongside its files; for a restore, those it loads back (set
	// when it starts: the snapshot's that the server still has).
	Databases []string `gorm:"serializer:json" json:"databases,omitempty"`
	// WithDatabases is a restore asked to load the snapshot's databases back,
	// and not only its files.
	WithDatabases bool `json:"withDatabases,omitempty"`

	// Ignored is, for a backup, the server's .quetzalignore as it read when
	// the backup started: the paths its snapshot left out. A restore of it
	// leaves those paths as they are on the volume.
	Ignored string `gorm:"type:text" json:"ignored,omitempty"`
	// Full is a backup of every file, whatever the server's .quetzalignore
	// says: a transfer's, whose snapshot becomes the server's whole volume on
	// another cluster.
	Full bool `json:"full,omitempty"`

	// A database import: the database it loads (a ServerDatabase ID), the file
	// of the server's it loads from, relative to the data volume's root, and
	// whether the database is emptied first.
	DatabaseID uint   `json:"databaseId,omitempty"`
	Path       string `json:"path,omitempty"`
	Wipe       bool   `json:"wipe,omitempty"`
}
