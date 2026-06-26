//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/backup"
	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// TestE2EBackupRestore runs a real backup and restore against an in-cluster
// MinIO via the restic runner Job: write a marker, back it up, delete it,
// restore, and confirm the marker is back.
func TestE2EBackupRestore(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	deployMinIO(ctx, t, cs)

	// Point the backup target at the in-cluster MinIO.
	if err := st.SaveBackupConfig(&models.BackupConfig{
		Endpoint: "minio.minio.svc:9000", Bucket: "quetzal", UseSSL: false, KeepLast: 3,
		RunnerImage: "restic/restic:0.17.3",
	}, "quetzaltest", "quetzaltest", "restic-test-pw"); err != nil {
		t.Fatalf("backup config: %v", err)
	}

	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-backup", DisplayName: "backup", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-backup"),
		DesiredState: models.StateRunning, Env: map[string]string{"MESSAGE": "hi"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	pod, err := console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod: %v", err)
	}
	const marker = "quetzal-backup-marker-OK"
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", "echo " + marker + " > /data/marker.txt"})

	mgr := backup.NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs, Config: cfg}))

	// Backup.
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, b.ID, models.BackupSucceeded, 4*time.Minute)
	done, _ := st.GetBackup(b.ID)
	if done.SizeBytes <= 0 {
		t.Errorf("backup size = %d, want > 0", done.SizeBytes)
	}

	// Destroy the data.
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", "rm -f /data/marker.txt"})
	out := execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", "cat /data/marker.txt 2>/dev/null || echo GONE"})
	if !bytes.Contains([]byte(out), []byte("GONE")) {
		t.Fatalf("marker should be gone before restore, got %q", out)
	}

	// A restore overwrites the volume in place, so the server must be stopped
	// first: the Manager refuses to restore while a pod still mounts the volume.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	waitNoPods(ctx, t, cs, srv.Namespace, srv.Slug)

	r := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: b.ID}
	if err := st.CreateBackup(r); err != nil {
		t.Fatalf("create restore: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, r.ID, models.BackupSucceeded, 4*time.Minute)

	// Start the server again; the restored marker must be back on the volume.
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatalf("start: %v", err)
	}
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)
	pod2, err := console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod after restart: %v", err)
	}
	out = execInPod(ctx, t, cs, cfg, srv.Namespace, pod2, []string{"sh", "-c", "cat /data/marker.txt"})
	if !bytes.Contains([]byte(out), []byte(marker)) {
		t.Fatalf("restore did not recover the marker; got %q", out)
	}
}

// waitNoPods blocks until no pod for the given server slug remains in the
// namespace (the data volume is fully released).
func waitNoPods(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, slug string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: reconciler.ServerLabel + "=" + slug,
		})
		if err != nil {
			return false, nil
		}
		return len(pods.Items) == 0, nil
	})
	if err != nil {
		t.Fatalf("pods for %s never terminated: %v", slug, err)
	}
}

func waitBackupPhase(ctx context.Context, t *testing.T, st *store.Store, mgr *backup.Manager, id uint, want models.BackupPhase, timeout time.Duration) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		mgr.Process(ctx)
		b, err := st.GetBackup(id)
		if err != nil {
			return false, err
		}
		if b.Phase == models.BackupFailed {
			return false, &backupFailed{b.Message}
		}
		return b.Phase == want, nil
	})
	if err != nil {
		t.Fatalf("backup %d never reached %s: %v", id, want, err)
	}
}

type backupFailed struct{ msg string }

func (e *backupFailed) Error() string { return "backup failed: " + e.msg }

// execInPod runs a command in the server container and returns combined stdout.
func execInPod(ctx context.Context, t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, cmd []string) string {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: reconciler.WorkloadName, Command: cmd, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		t.Fatalf("exec init: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		t.Fatalf("exec %v: %v (stderr: %s)", cmd, err, stderr.String())
	}
	return stdout.String()
}

// deployMinIO stands up a single-node MinIO with a pre-created "quetzal" bucket.
func deployMinIO(ctx context.Context, t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	ns := "minio"
	_, _ = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	labels := map[string]string{"app": "minio"}
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "minio",
					Image: "minio/minio:RELEASE.2024-10-13T13-34-11Z",
					// Pre-create the bucket as a directory so restic finds it.
					Command: []string{"sh", "-c", "mkdir -p /data/quetzal && minio server /data --console-address :9001"},
					Env: []corev1.EnvVar{
						{Name: "MINIO_ROOT_USER", Value: "quetzaltest"},
						{Name: "MINIO_ROOT_PASSWORD", Value: "quetzaltest"},
					},
					Ports: []corev1.ContainerPort{{ContainerPort: 9000}},
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("minio deploy: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 9000, TargetPort: intstr.FromInt32(9000)}},
		},
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("minio svc: %v", err)
	}
	// Wait for MinIO to be ready.
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, "minio", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return d.Status.ReadyReplicas >= 1, nil
	})
	if err != nil {
		t.Fatalf("minio not ready: %v", err)
	}
}
