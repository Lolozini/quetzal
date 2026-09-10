package backup

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestServerHasPodsDetectsDataPod guards the RWO-safety invariant: a restore
// must be deferred while ANY pod mounts the data volume — including the always-on
// data-manager pod, which carries DataLabel (not ServerLabel). If this check
// missed it, a restore could run concurrently with the data-manager and corrupt
// the volume (the reconciler scales the data-manager down during a restore).
func TestServerHasPodsDetectsDataPod(t *testing.T) {
	const ns, slug = "quetzal-srv-s1", "s1"
	mkPod := func(name, labelKey string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Labels: map[string]string{labelKey: slug},
		}}
	}

	// Only the data-manager pod present -> must report true.
	cs := fake.NewSimpleClientset(mkPod("data-manager-abc", reconciler.DataLabel))
	if has, err := serverHasPods(context.Background(), cs, ns, slug); err != nil || !has {
		t.Fatalf("data-only: has=%v err=%v, want true", has, err)
	}

	// Only the workload pod present -> true (existing behavior).
	cs = fake.NewSimpleClientset(mkPod("server-abc", reconciler.ServerLabel))
	if has, err := serverHasPods(context.Background(), cs, ns, slug); err != nil || !has {
		t.Fatalf("workload-only: has=%v err=%v, want true", has, err)
	}

	// Only an activator pod (does not mount data) -> false.
	cs = fake.NewSimpleClientset(mkPod("activator-xyz", reconciler.ActivatorLabel))
	if has, err := serverHasPods(context.Background(), cs, ns, slug); err != nil || has {
		t.Fatalf("activator-only: has=%v err=%v, want false", has, err)
	}

	// Nothing -> false.
	cs = fake.NewSimpleClientset()
	if has, err := serverHasPods(context.Background(), cs, ns, slug); err != nil || has {
		t.Fatalf("empty: has=%v err=%v, want false", has, err)
	}
}

func TestRepository(t *testing.T) {
	cfg := &models.BackupConfig{Endpoint: "minio.minio.svc:9000", Bucket: "quetzal", Prefix: "/games/", UseSSL: false}
	// Per-server repository: prefix + slug appended.
	if got, want := Repository(cfg, "s1"), "s3:http://minio.minio.svc:9000/quetzal/games/s1"; got != want {
		t.Errorf("repo = %q, want %q", got, want)
	}
	cfg.UseSSL = true
	cfg.Prefix = ""
	if got, want := Repository(cfg, "valheim"), "s3:https://minio.minio.svc:9000/quetzal/valheim"; got != want {
		t.Errorf("repo = %q, want %q", got, want)
	}
}

func TestBuildJobDataVolume(t *testing.T) {
	// Data is always the server's PVC.
	pvc := BuildJob(Params{Slug: "s1", BackupID: 1, Direction: models.DirBackup})
	if v := pvc.Spec.Template.Spec.Volumes[0]; v.PersistentVolumeClaim == nil {
		t.Errorf("expected PVC volume, got %+v", v)
	}
}

func TestBuildJobCoLocation(t *testing.T) {
	ns := map[string]string{"disktype": "ssd"}
	// Backup co-locates with the data-manager (which still holds the RWO volume).
	b := BuildJob(Params{Slug: "s1", BackupID: 1, Direction: models.DirBackup, NodeSelector: ns})
	ps := b.Spec.Template.Spec
	if ps.NodeSelector["disktype"] != "ssd" {
		t.Errorf("backup NodeSelector = %v, want it propagated", ps.NodeSelector)
	}
	if ps.Affinity == nil || ps.Affinity.PodAffinity == nil ||
		len(ps.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("backup Job must co-locate (podAffinity) with the data-manager")
	}
	term := ps.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.LabelSelector.MatchLabels[reconciler.DataLabel] != "s1" || term.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("backup affinity term = %+v", term)
	}
	// Restore runs after all data pods are gone (volume free) -> no podAffinity.
	r := BuildJob(Params{Slug: "s1", BackupID: 2, Direction: models.DirRestore, SourceID: 1, NodeSelector: ns})
	if r.Spec.Template.Spec.Affinity != nil {
		t.Errorf("restore Job should have no podAffinity, got %+v", r.Spec.Template.Spec.Affinity)
	}
	if r.Spec.Template.Spec.NodeSelector["disktype"] != "ssd" {
		t.Errorf("restore NodeSelector = %v, want it propagated", r.Spec.Template.Spec.NodeSelector)
	}
}

func TestBuildJobBackup(t *testing.T) {
	p := Params{
		Image: "restic/restic:test", Namespace: "quetzal-srv-s1", Slug: "s1",
		BackupID: 42, Direction: models.DirBackup, KeepLast: 5,
	}
	job := BuildJob(p)
	if job.Name != "quetzal-backup-42" {
		t.Errorf("job name = %q", job.Name)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "restic/restic:test" {
		t.Errorf("image = %q", c.Image)
	}
	script := c.Command[2]
	for _, want := range []string{"restic backup /data", `--tag "bid-42"`, "restic forget", "--keep-last 5", "restic init"} {
		if !strings.Contains(script, want) {
			t.Errorf("backup script missing %q:\n%s", want, script)
		}
	}
	// Data volume mounted read-only from the server's PVC.
	vm := c.VolumeMounts[0]
	if vm.MountPath != "/data" || !vm.ReadOnly {
		t.Errorf("volume mount = %+v, want /data read-only", vm)
	}
	if v := job.Spec.Template.Spec.Volumes[0]; v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "data" {
		t.Errorf("volume = %+v, want PVC data", v)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil {
		t.Errorf("env should come from the creds secret: %+v", c.EnvFrom)
	}
}

func TestBuildJobRestore(t *testing.T) {
	p := Params{Slug: "s1", BackupID: 7, SourceID: 3, Direction: models.DirRestore}
	job := BuildJob(p)
	if job.Name != "quetzal-restore-7" {
		t.Errorf("job name = %q", job.Name)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	for _, want := range []string{"restic restore latest", `--tag "bid-3"`, "--target /"} {
		if !strings.Contains(script, want) {
			t.Errorf("restore script missing %q:\n%s", want, script)
		}
	}
	// Restore needs write access.
	if job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Error("restore volume must be writable")
	}
}

func TestBuildSecret(t *testing.T) {
	p := Params{Namespace: "ns", Repository: "s3:http://x/b", RepoPassword: "pw", AccessKey: "ak", SecretKey: "sk", Region: "gra"}
	sec := BuildSecret(p)
	if sec.Name != CredsSecretName || sec.Namespace != "ns" {
		t.Errorf("secret meta = %s/%s", sec.Namespace, sec.Name)
	}
	want := map[string]string{
		"RESTIC_REPOSITORY": "s3:http://x/b", "RESTIC_PASSWORD": "pw",
		"AWS_ACCESS_KEY_ID": "ak", "AWS_SECRET_ACCESS_KEY": "sk", "AWS_DEFAULT_REGION": "gra",
	}
	for k, v := range want {
		if sec.StringData[k] != v {
			t.Errorf("secret[%s] = %q, want %q", k, sec.StringData[k], v)
		}
	}
}

func TestParseBackupSize(t *testing.T) {
	logs := `{"message_type":"status","percent_done":0.5}
{"message_type":"summary","files_new":3,"total_bytes_processed":1048576,"snapshot_id":"abc"}
`
	if got := ParseBackupSize(logs); got != 1048576 {
		t.Errorf("size = %d, want 1048576", got)
	}
	if got := ParseBackupSize("no json here"); got != 0 {
		t.Errorf("size = %d, want 0", got)
	}
}

// A forget only rewrites the repository: it must mount nothing (so it never
// contends for the ReadWriteOnce data volume), carry no node placement, target
// exactly the snapshot being deleted, and get a Job name of its own.
func TestBuildJobForget(t *testing.T) {
	p := Params{
		Slug: "s1", BackupID: 7, Direction: models.DirBackup, Forget: true,
		NodeSelector: map[string]string{"disktype": "ssd"},
	}
	j := BuildJob(p)
	ps := j.Spec.Template.Spec
	if len(ps.Volumes) != 0 {
		t.Errorf("forget Job mounts %d volume(s), want none", len(ps.Volumes))
	}
	if len(ps.Containers[0].VolumeMounts) != 0 {
		t.Errorf("forget container has mounts: %+v", ps.Containers[0].VolumeMounts)
	}
	if ps.Affinity != nil {
		t.Errorf("forget Job should have no podAffinity, got %+v", ps.Affinity)
	}
	if ps.NodeSelector != nil {
		t.Errorf("forget Job should not be pinned to a node, got %v", ps.NodeSelector)
	}
	script := ps.Containers[0].Command[2]
	if !strings.Contains(script, `--tag "bid-7"`) || !strings.Contains(script, "forget") || !strings.Contains(script, "--prune") {
		t.Errorf("forget script does not remove the tagged snapshot:\n%s", script)
	}
	// restic rejects a forget with no retention policy, whatever the filters
	// select: "no policy was specified, no snapshots will be removed", exit 1.
	// Asserting the tag and --prune are present is not enough — that is what the
	// first version of this test did, and the command never worked once.
	if !strings.Contains(script, "--unsafe-allow-remove-all") {
		t.Errorf("forget script has no policy and no --unsafe-allow-remove-all, so restic will refuse it:\n%s", script)
	}
	if strings.Contains(script, "restic backup") || strings.Contains(script, "restic restore") {
		t.Errorf("forget script should not back up or restore:\n%s", script)
	}
	if got, want := JobName(p), "quetzal-forget-7"; got != want {
		t.Errorf("JobName = %q, want %q (must not collide with the backup Job)", got, want)
	}
}

// A backup that fails is only actionable if the record says why. When a Job
// retries, the newest attempt can die before its container starts — a missing
// secret, an image that will not pull — and produce no logs at all, which used
// to leave the user with a bare "backup job failed". The reason Kubernetes
// recorded on the pod is then the whole story.
func TestContainerFailureExplainsAPodThatNeverRan(t *testing.T) {
	waiting := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CreateContainerConfigError",
				Message: `secret "quetzal-backup-creds" not found`,
			}},
		}},
	}}
	got := containerFailure(waiting)
	if !strings.Contains(got, "quetzal-backup-creds") || !strings.Contains(got, "CreateContainerConfigError") {
		t.Errorf("waiting reason = %q, want the reason and the message", got)
	}

	terminated := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137,
			}},
		}},
	}}
	if got := containerFailure(terminated); !strings.Contains(got, "OOMKilled") {
		t.Errorf("terminated reason = %q, want OOMKilled", got)
	}

	// A container that ran and exited cleanly has nothing to add: whatever
	// happened is in the logs, and inventing a reason here would mask them.
	ok := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Completed", ExitCode: 0,
			}},
		}},
	}}
	if got := containerFailure(ok); got != "" {
		t.Errorf("completed container = %q, want empty", got)
	}
}

// A restore has to leave the volume matching the snapshot. Without --delete a
// file created after the snapshot survives, so rolling a server back after a bad
// mod install kept the mod's files — defeating the point of the restore, and
// contradicting what the panel tells the user is about to happen.
//
// The include filter is not cosmetic: restic refuses "--target / --delete"
// without one, because the snapshot's root holds only the data directory and an
// unfiltered delete would take that directory's siblings at / with it.
func TestBuildJobRestoreMatchesTheSnapshot(t *testing.T) {
	j := BuildJob(Params{Slug: "s1", BackupID: 9, SourceID: 4, Direction: models.DirRestore})
	script := j.Spec.Template.Spec.Containers[0].Command[2]
	if !strings.Contains(script, "restic restore") || !strings.Contains(script, `--tag "bid-4"`) {
		t.Fatalf("not a restore of the source snapshot:\n%s", script)
	}
	if !strings.Contains(script, "--delete") {
		t.Errorf("restore merges into the volume instead of matching the snapshot:\n%s", script)
	}
	if !strings.Contains(script, "--include "+mountPath) {
		t.Errorf("restore deletes without scoping to the data path, which restic refuses:\n%s", script)
	}
	if !strings.Contains(script, "--target /") {
		t.Errorf("restore does not target / (restic stores absolute paths):\n%s", script)
	}
}

// Deleting a server used to drop its backup rows and leave every snapshot in the
// bucket: unreachable from the panel, and billed for ever. The purge Job runs in
// the control plane's namespace because the server's is being torn down, which it
// can only do by touching nothing but the repository.
func TestBuildJobPurge(t *testing.T) {
	p := Params{
		Slug: "s1", Namespace: "quetzal", Purge: true,
		NodeSelector: map[string]string{"disktype": "ssd"},
	}
	j := BuildJob(p)
	if j.Name != "quetzal-purge-s1" {
		t.Errorf("job name = %q, want it named after the server (its backup rows are gone)", j.Name)
	}
	if j.Namespace != "quetzal" {
		t.Errorf("job namespace = %q, want the control plane's", j.Namespace)
	}
	ps := j.Spec.Template.Spec
	if len(ps.Volumes) != 0 || len(ps.Containers[0].VolumeMounts) != 0 {
		t.Errorf("purge Job touches the data volume, so it cannot outlive the namespace: %+v", ps.Volumes)
	}
	if ps.NodeSelector != nil || ps.Affinity != nil {
		t.Errorf("purge Job is pinned to the server's placement: %v %+v", ps.NodeSelector, ps.Affinity)
	}
	if got := SecretName(p); got != CredsSecretName+"-s1" {
		t.Errorf("purge creds secret = %q, want a per-server name (purges share one namespace)", got)
	}
	script := ps.Containers[0].Command[2]
	if !strings.Contains(script, `--host "s1"`) || !strings.Contains(script, "--unsafe-allow-remove-all") ||
		!strings.Contains(script, "--prune") {
		t.Errorf("purge does not drop every snapshot for the server:\n%s", script)
	}
	if strings.Contains(script, "--tag") {
		t.Errorf("purge filters by tag, so it would leave the other snapshots behind:\n%s", script)
	}
	// A server deleted before its first backup has no repository at all; that is
	// not a failure to report.
	if !strings.Contains(script, "restic snapshots >/dev/null 2>&1 || exit 0") {
		t.Errorf("purge fails on a repository that was never initialised:\n%s", script)
	}
}
