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
