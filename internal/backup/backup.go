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
}

// Repository builds a restic S3 repository URL from a backup config.
func Repository(cfg *models.BackupConfig) string {
	scheme := "https://"
	if !cfg.UseSSL {
		scheme = "http://"
	}
	repo := "s3:" + scheme + cfg.Endpoint + "/" + cfg.Bucket
	if p := strings.Trim(cfg.Prefix, "/"); p != "" {
		repo += "/" + p
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

// JobName is the deterministic Job name for an operation.
func JobName(p Params) string {
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
	switch p.Direction {
	case models.DirRestore:
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
	ttl := int32(1800) // safety net; the controller deletes finished Jobs itself
	ro := p.Direction == models.DirBackup

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
					Containers: []corev1.Container{{
						Name:    "restic",
						Image:   p.Image,
						Command: []string{"/bin/sh", "-c", script},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: CredsSecretName},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: mountPath, ReadOnly: ro}},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: reconciler.DataVolume,
								ReadOnly:  ro,
							},
						},
					}},
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
