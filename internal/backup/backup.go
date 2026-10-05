// Package backup builds the Kubernetes objects that back up and restore a
// server's data volume with restic, to any S3-compatible target. It is
// provider-neutral (endpoint/bucket/credentials/password are all configurable)
// and uses no sidecar: each operation is a one-shot Job that mounts the data
// PVC. restic provides encryption, deduplication, retention and restore.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
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
	defaultImage = "restic/restic:0.19.1"
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
	// InstallGen is the server's install generation, written into the install
	// marker after a restore (see the restore script).
	InstallGen int
	// Purge drops every snapshot the server has, for when the server itself is
	// deleted. Like Forget it only talks to the repository — which also means it
	// has no affinity to the server's cluster or namespace, so it can run in the
	// control plane's own namespace and outlive the namespace being torn down.
	Purge bool
	// Databases are the server's databases a backup dumps into its snapshot,
	// a restore loads back, or -- the one -- an import loads a file into.
	Databases []DatabaseParams
	// ImportPath is the file an import loads, relative to the volume's root;
	// ImportWipe empties the database first.
	ImportPath string
	ImportWipe bool
}

// repoOnly reports whether the operation touches only the restic repository, and
// so needs no data volume, no node placement and no co-location.
func (p Params) repoOnly() bool { return p.Forget || p.Purge }

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

// TargetID fingerprints where a backup target keeps its snapshots: the
// endpoint, bucket and prefix that every server's repository URL is built
// from. Each backup records it, so a backup made to a target the panel has
// since left is known as such. A fingerprint rather than the location itself:
// a backup's record is readable by anyone who can view its server, and where
// the target is stays the administrators' business.
func TargetID(cfg *models.BackupConfig) string {
	loc := strings.ToLower(strings.TrimSpace(cfg.Endpoint)) + "\x00" +
		strings.TrimSpace(cfg.Bucket) + "\x00" + strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	sum := sha256.Sum256([]byte(loc))
	return hex.EncodeToString(sum[:8])
}

// OtherTargetMessage explains why a backup made to another target cannot be
// restored.
const OtherTargetMessage = "this backup was made to a backup target the panel no longer uses; it can be restored once that target is set again"

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
	if p.Purge {
		// Named after the server, not a backup: the rows are already gone by the
		// time this runs. Slugs carry a random suffix and are never reused, so it
		// stays unique in the shared namespace it runs in.
		return fmt.Sprintf("quetzal-purge-%s", p.Slug)
	}
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

// SecretName is the Secret holding an operation's restic credentials. Backups,
// restores and single-snapshot deletions run in the server's own namespace and
// can share one name. A purge runs in the control plane's namespace, alongside
// the purges of other servers, and each carries a different RESTIC_REPOSITORY —
// so it gets a name of its own rather than fighting over the shared one.
func SecretName(p Params) string {
	if p.Purge {
		return CredsSecretName + "-" + p.Slug
	}
	return CredsSecretName
}

// BuildSecret renders the credentials Secret for an operation: restic's, and
// the password of each database it works on. An import talks to its database
// only, and gets nothing of the backup target.
func BuildSecret(p Params) *corev1.Secret {
	data := map[string]string{}
	if !importsDirection(p) {
		data = map[string]string{
			"RESTIC_REPOSITORY":     p.Repository,
			"RESTIC_PASSWORD":       p.RepoPassword,
			"AWS_ACCESS_KEY_ID":     p.AccessKey,
			"AWS_SECRET_ACCESS_KEY": p.SecretKey,
			"AWS_DEFAULT_REGION":    p.Region,
		}
	}
	for i, db := range p.Databases {
		data[dbPasswordKey(i)] = db.Password
	}
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: SecretName(p), Namespace: p.Namespace, Labels: labels(p)},
		StringData: data,
	}
}

// BuildJob renders the one-shot Job for an operation: restic for a backup, a
// restore or a snapshot deletion, the MariaDB client for a database import. A
// backup of a server with databases dumps them first, in init containers, and
// restic copies the dumps with the files; a restore asked for them extracts
// the dumps with the files, and init containers load them back after restic.
func BuildJob(p Params) *batchv1.Job {
	tag := fmt.Sprintf("bid-%d", p.BackupID)
	var script string
	switch {
	case p.Purge:
		// Drop every snapshot this server has, for when the server itself is
		// deleted: its repository is per-server, so nothing else lives in it.
		//
		// A repository that was never initialised (no backup ever ran) is not an
		// error, and restic says so specifically: exit 10 means the repository
		// does not exist. Anything else — wrong credentials is exit 1 — is a real
		// failure and has to stay one. Treating every non-zero code as "nothing
		// to purge" would report success while the snapshots are still there,
		// which is the outcome this whole job exists to prevent. stderr is left
		// alone so the reason reaches the pod log.
		script = fmt.Sprintf(`set -e
rc=0
restic snapshots >/dev/null || rc=$?
[ "$rc" = 10 ] && exit 0
[ "$rc" = 0 ] || exit "$rc"
restic forget --host %q --unsafe-allow-remove-all --prune
`, p.Slug)
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
		//
		// --delete makes the volume match the snapshot instead of merging into it.
		// Without it a file created after the snapshot survives, so rolling a
		// server back after a bad mod install left the mod's files in place — the
		// exact thing the restore was for, and not what the panel promises
		// ("current data will be overwritten by the snapshot").
		//
		// --include is mandatory, not decoration: restic refuses "--target /
		// --delete" without a filter ("must be combined with an include or exclude
		// filter"), and rightly so — the snapshot's root holds only the data
		// directory, so an unfiltered delete would take that directory's siblings
		// at / with it. Scoping the deletion to the data path keeps it to the
		// volume.
		//
		// The restored data then counts as installed, as it does on Pterodactyl:
		// the snapshot carries the install marker of its day, and a snapshot
		// older than the last reinstall carried an older generation, so the next
		// start re-ran the install over the data that had just been restored
		// (wiping it first, when that reinstall had asked for a wipe). The marker
		// keeps the owner it was restored with; a new one goes to the owner of
		// the volume's root, which is the server's user.
		script = fmt.Sprintf(`set -e
restic restore latest --host %q --tag %q --target / --delete --include %s
marker=%s/.quetzal-installed
printf '%%s' %q > "$marker"
chown "$(stat -c %%u:%%g %s)" "$marker" 2>/dev/null || true
`, p.Slug, srcTag, mountPath, mountPath, strconv.Itoa(p.InstallGen), mountPath)
		if len(p.Databases) > 0 {
			// The dumps go beside the volume, for the load steps that follow.
			script += fmt.Sprintf("restic restore latest --host %q --tag %q --target %s --include %s\n",
				p.Slug, srcTag, restorePath, dumpsPath)
		}
	default: // backup
		keep := p.KeepLast
		if keep <= 0 {
			keep = 7
		}
		// The first backup of a server creates its repository, and restic says
		// when there is none: exit 10. Any other failure is the answer, with its
		// own error. This used to be "snapshots || init", which hid the error of
		// the first command, then waited again for the second on a target that
		// never answered, and reported "create repository ... failed" whatever
		// had gone wrong -- a refused key, a wrong password, a missing bucket.
		script = fmt.Sprintf(`set -e
rc=0
restic snapshots >/dev/null || rc=$?
if [ "$rc" = 10 ]; then
  restic init
elif [ "$rc" != 0 ]; then
  exit "$rc"
fi
restic backup %s --host %q --tag quetzal --tag %q --json
restic forget --host %q --keep-last %d --prune
`, backupPaths(p), p.Slug, tag, p.Slug, keep)
	}

	backoff := int32(1)
	// Safety net only: the controller deletes finished Jobs itself. It is kept
	// long because a Job that vanishes before the controller has read its result
	// is reported as a failure — a day gives an offline or non-leader controller
	// ample room to come back and see that the operation actually succeeded.
	ttl := int32(86400)
	// A Job with no deadline can stall forever -- a bucket that stopped answering
	// mid-transfer leaves restic waiting, the operation never finishes, and a
	// transfer built on it pins its server indefinitely. Six hours is far more
	// than any real backup or restore needs (a hundred gigabytes at a handful of
	// megabytes a second) and far less than never, and hitting it fails the
	// operation with a reason rather than leaving it hanging.
	deadline := int64(6 * 60 * 60)

	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: JobName(p), Namespace: p.Namespace, Labels: labels(p)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(p)},
				Spec:       podSpec(p, script),
			},
		},
	}
}

// backupPaths is what a backup copies: the volume, and the dumps of the
// server's databases when it has some.
func backupPaths(p Params) string {
	if len(p.Databases) > 0 {
		return mountPath + " " + dumpsPath
	}
	return mountPath
}

// podSpec lays out an operation's pod around its restic script.
func podSpec(p Params, script string) corev1.PodSpec {
	ro := p.Direction == models.DirBackup || importsDirection(p)

	// Mount the same data the server uses: its PVC. A forget only rewrites the
	// repository, so it takes no volume at all — which also keeps it off the
	// ReadWriteOnce mount and lets it run while the server is up.
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if !p.repoOnly() {
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
	// ReadWriteOnce mount, so the Job must land on the same node -- and so does
	// an import, which reads its file from there. A restore runs only after
	// every data-mounting pod is gone (the manager defers it), so the volume is
	// free and node placement is left to the volume/NodeSelector. A forget
	// mounts nothing, so it must not inherit the volume's node pinning.
	nodeSelector := p.NodeSelector
	if p.repoOnly() {
		nodeSelector = nil
	}
	var affinity *corev1.Affinity
	if ro && !p.repoOnly() {
		affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{reconciler.DataLabel: p.Slug}},
				TopologyKey:   "kubernetes.io/hostname",
			}},
		}}
	}
	// Nothing here talks to the Kubernetes API.
	noToken := false
	spec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		NodeSelector:                 nodeSelector,
		Affinity:                     affinity,
		AutomountServiceAccountToken: &noToken,
		Volumes:                      volumes,
	}
	restic := corev1.Container{
		Name:    "restic",
		Image:   p.Image,
		Command: []string{"/bin/sh", "-c", script},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(p)},
			},
		}},
		VolumeMounts: mounts,
	}

	switch {
	case importsDirection(p):
		spec.Containers = []corev1.Container{importContainer(p, mounts)}
	case p.repoOnly() || len(p.Databases) == 0:
		spec.Containers = []corev1.Container{restic}
	case p.Direction == models.DirBackup:
		// The dumps first, then restic copies them with the files.
		spec.Volumes = append(spec.Volumes, dumpsVolumeSource())
		spec.InitContainers = dumpContainers(p)
		restic.VolumeMounts = append(restic.VolumeMounts, corev1.VolumeMount{Name: dumpsVolume, MountPath: dumpsPath, ReadOnly: true})
		spec.Containers = []corev1.Container{restic}
	default:
		// restic first -- the files, and the dumps beside them -- then the
		// databases, one after the other. Init containers run in order and
		// stop at the first that fails; the pod's own container only marks the
		// end of it.
		spec.Volumes = append(spec.Volumes, dumpsVolumeSource())
		restic.VolumeMounts = append(restic.VolumeMounts, corev1.VolumeMount{Name: dumpsVolume, MountPath: restorePath})
		spec.InitContainers = append([]corev1.Container{restic}, loadContainers(p)...)
		spec.Containers = []corev1.Container{{
			Name:    "done",
			Image:   p.Image,
			Command: []string{"/bin/sh", "-c", "echo restored"},
		}}
	}
	return spec
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
