// Package backup builds the Kubernetes objects that back up and restore a
// server's data volume with restic, to any S3-compatible target. It is
// provider-neutral (endpoint/bucket/credentials/password are all configurable)
// and uses no sidecar: each operation is a one-shot Job that mounts the data
// PVC. restic provides encryption, deduplication, retention and restore.
package backup

import (
	"encoding/json"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

const (
	// CredsSecretName is the per-namespace Secret holding restic credentials.
	CredsSecretName = "quetzal-backup-creds"
	// BackupLabel marks backup Jobs/Secrets (value = backup operation ID).
	BackupLabel  = "quetzal.dev/backup"
	mountPath    = "/data"
	defaultImage = "restic/restic:0.17.3"
)

// Params is everything needed to render a backup/restore operation.
type Params struct {
	Image        string
	Namespace    string
	Slug         string
	BackupID     uint
	Direction    models.BackupDirection
	SourceID     uint // restore: the backup ID to restore from
	KeepLast     int
	Repository   string // restic repository URL (s3:...)
	Region       string
	AccessKey    string
	SecretKey    string
	RepoPassword string
	// NodeSelector co-locates the Job with the server's pods (it mounts the same
	// ReadWriteOnce data PVC), mirroring the game/data-manager placement.
	NodeSelector map[string]string
	// Forget turns the operation into a snapshot deletion: restic drops the
	// snapshot tagged with BackupID from the repository. It touches only the
	// repository, so unlike a backup or restore it mounts no data volume and
	// needs no node placement.
	Forget bool
}

// Repository builds a restic S3 repository URL for a server. Each server gets
// its own repository (…/<prefix>/<slug>) so concurrent backups of different
// servers never contend on restic's exclusive forget/prune lock.
func Repository(cfg *models.BackupConfig, slug string) string {
	scheme := "https://"
	if !cfg.UseSSL {
		scheme = "http://"
	}
	repo := "s3:" + scheme + cfg.Endpoint + "/" + cfg.Bucket
	if p := strings.Trim(cfg.Prefix, "/"); p != "" {
		repo += "/" + p
	}
	if slug != "" {
		repo += "/" + slug
	}
	return repo
}

// Image returns the runner image, defaulting when unset.
func Image(cfg *models.BackupConfig) string {
	if cfg.RunnerImage != "" {
		return cfg.RunnerImage
	}
	return defaultImage
}

// JobName is the deterministic Job name for an operation. A forget gets its own
// name so it never collides with the backup Job that created the snapshot.
func JobName(p Params) string {
	if p.Forget {
		return fmt.Sprintf("quetzal-forget-%d", p.BackupID)
	}
	return fmt.Sprintf("quetzal-%s-%d", p.Direction, p.BackupID)
}

func labels(p Params) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "quetzal",
		reconciler.ServerLabel:         p.Slug,
		BackupLabel:                    fmt.Sprintf("%d", p.BackupID),
	}
}

// BuildSecret renders the restic credentials Secret for an operation.
func BuildSecret(p Params) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: CredsSecretName, Namespace: p.Namespace, Labels: labels(p)},
		StringData: map[string]string{
			"RESTIC_REPOSITORY":     p.Repository,
			"RESTIC_PASSWORD":       p.RepoPassword,
			"AWS_ACCESS_KEY_ID":     p.AccessKey,
			"AWS_SECRET_ACCESS_KEY": p.SecretKey,
			"AWS_DEFAULT_REGION":    p.Region,
		},
	}
}

// BuildJob renders the one-shot restic Job for a backup or restore.
func BuildJob(p Params) *batchv1.Job {
	tag := fmt.Sprintf("bid-%d", p.BackupID)
	var script string
	switch {
	case p.Forget:
		// Drop this one snapshot and reclaim its space. --prune is what actually
		// removes the data from the bucket; forgetting alone only unlinks it.
		//
		// --unsafe-allow-remove-all is required, not optional: restic refuses a
		// forget that carries no retention policy ("no policy was specified, no
		// snapshots will be removed") even when the filters select a single
		// snapshot, and exits non-zero. The filters are what make it safe here —
		// the tag is the backup's primary key, so it names exactly one snapshot,
		// and --host scopes it to this server's.
		//
		// An already-absent snapshot is not an error: a tag that matches nothing
		// still exits 0, which keeps the delete idempotent on retry.
		script = fmt.Sprintf(`set -e
restic forget --host %q --tag %q --unsafe-allow-remove-all --prune
`, p.Slug, tag)
	case p.Direction == models.DirRestore:
		srcTag := fmt.Sprintf("bid-%d", p.SourceID)
		// Restore the snapshot tagged with the source backup into the PVC. restic
		// stores absolute paths, so target "/" recreates /data.
		script = fmt.Sprintf(`set -e
restic restore latest --host %q --tag %q --target /
`, p.Slug, srcTag)
	default: // backup
		keep := p.KeepLast
		if keep <= 0 {
			keep = 7
		}
		script = fmt.Sprintf(`set -e
restic snapshots >/dev/null 2>&1 || restic init
restic backup %s --host %q --tag quetzal --tag %q --json
restic forget --host %q --keep-last %d --prune
`, mountPath, p.Slug, tag, p.Slug, keep)
	}

	backoff := int32(1)
	// Safety net only: the controller deletes finished Jobs itself. It is kept
	// long because a Job that vanishes before the controller has read its result
	// is reported as a failure — a day gives an offline or non-leader controller
	// ample room to come back and see that the operation actually succeeded.
	ttl := int32(86400)
	ro := p.Direction == models.DirBackup

	// Mount the same data the server uses: its PVC. A forget only rewrites the
	// repository, so it takes no volume at all — which also keeps it off the
	// ReadWriteOnce mount and lets it run while the server is up.
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if !p.Forget {
		volumes = []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: reconciler.DataVolume,
					ReadOnly:  ro,
				},
			},
		}}
		mounts = []corev1.VolumeMount{{Name: "data", MountPath: mountPath, ReadOnly: ro}}
	}

	// A backup mounts the volume read-only while the data-manager still holds the
	// ReadWriteOnce mount, so the Job must land on the same node. A restore runs
	// only after every data-mounting pod is gone (the manager defers it), so the
	// volume is free and node placement is left to the volume/NodeSelector.
	// A forget mounts nothing, so it must not inherit the volume's node pinning.
	nodeSelector := p.NodeSelector
	if p.Forget {
		nodeSelector = nil
	}
	var affinity *corev1.Affinity
	if p.Direction == models.DirBackup && !p.Forget {
		affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{reconciler.DataLabel: p.Slug}},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		}}
	}

	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: JobName(p), Namespace: p.Namespace, Labels: labels(p)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(p)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  nodeSelector,
					Affinity:      affinity,
					Containers: []corev1.Container{{
						Name:    "restic",
						Image:   p.Image,
						Command: []string{"/bin/sh", "-c", script},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: CredsSecretName},
							},
						}},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// ParseBackupSize extracts the total bytes processed from restic's --json
// backup output (the "summary" line), or 0 if not found.
func ParseBackupSize(logs string) int64 {
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, "summary") {
			continue
		}
		var s struct {
			MessageType string `json:"message_type"`
			TotalBytes  int64  `json:"total_bytes_processed"`
		}
		if json.Unmarshal([]byte(line), &s) == nil && s.MessageType == "summary" {
			return s.TotalBytes
		}
	}
	return 0
}
