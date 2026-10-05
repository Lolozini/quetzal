//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
)

// mariadbFixtureImage is the database the test backs up. CI preloads the same
// tag into kind (see ci.yml); it is also the client the Jobs run.
const mariadbFixtureImage = reconciler.DefaultMariaDBImage

// TestE2EDatabaseBackupRestoreImport backs a server up with its database, as
// its Jobs run in a cluster -- the dump in an init container, restic copying it
// with the files to the S3 fixture -- changes the database, restores the
// backup with its databases, and loads an SQL file of the server's into it.
// The database is a MariaDB of its own namespace, registered as an external
// host, its database and account made by the image as Quetzal would make them.
func TestE2EDatabaseBackupRestoreImport(t *testing.T) {
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
	if err := st.SaveBackupConfig(&models.BackupConfig{
		Endpoint: "minio.minio.svc:9000", Bucket: "quetzal", UseSSL: false, KeepLast: 3,
		RunnerImage: "restic/restic:0.19.1",
	}, "quetzaltest", "quetzaltest", "restic-test-pw"); err != nil {
		t.Fatalf("backup config: %v", err)
	}

	const dbName, user, password = "s9_e2edbbak", "u9_e2edbbak", "Passw0rdPassw0rdPassw0rd"
	ns := fmt.Sprintf("e2e-mariadb-%d", time.Now().Unix()%100000)
	dbPod := deployMariaDB(ctx, t, cs, ns, dbName, user, password)
	sql := func(stmts string) string {
		t.Helper()
		return execIn(ctx, t, cs, cfg, ns, dbPod, "mariadb", []string{"mariadb", "-N", "-B", "-u" + user, "-p" + password, dbName, "-e", stmts})
	}
	sql("CREATE TABLE t (v VARCHAR(32)); INSERT INTO t VALUES ('before');")

	host := &models.DatabaseHost{Name: "e2e", Kind: models.DBHostExternal, Host: "mariadb." + ns + ".svc", Port: 3306, AdminUser: "root"}
	if err := st.CreateDatabaseHost(host, "rootpw"); err != nil {
		t.Fatalf("host: %v", err)
	}
	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-dbbak", DisplayName: "dbbak", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-dbbak"),
		DesiredState: models.StateRunning, Env: map[string]string{"MESSAGE": "hi"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	db := &models.ServerDatabase{ServerID: srv.ID, HostID: host.ID, DatabaseName: dbName, Username: user, Remote: "%"}
	if err := st.CreateServerDatabase(db, password); err != nil {
		t.Fatalf("server database: %v", err)
	}
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	// The file the import loads, uploaded as a user would, before the backup:
	// the restore makes the volume the backup's.
	pod, err := console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod: %v", err)
	}
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c",
		"printf 'USE `elsewhere`;\\nCREATE TABLE imported (x INT);\\nINSERT INTO imported VALUES (42);\\n' > /data/dump.sql"})

	mgr := backup.NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs, Config: cfg}))
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, rec, srv.ID, b.ID, models.BackupSucceeded, 5*time.Minute)
	if done, _ := st.GetBackup(b.ID); len(done.Databases) != 1 || done.Databases[0] != dbName {
		t.Fatalf("the backup holds databases %v, want %s", done.Databases, dbName)
	}

	// The game goes on.
	sql("UPDATE t SET v = 'after'; CREATE TABLE later (x INT);")

	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	waitNoPods(ctx, t, cs, srv.Namespace, srv.Slug)
	r := &models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending, SourceID: b.ID, WithDatabases: true}
	if err := st.CreateRestore(r); err != nil {
		t.Fatalf("create restore: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, rec, srv.ID, r.ID, models.BackupSucceeded, 5*time.Minute)
	if got := sql("SELECT v FROM t; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE();"); strings.TrimSpace(got) != "before\n1" {
		t.Fatalf("after the restore the database holds %q, want the backup's (before, one table)", got)
	}

	// The SQL file of the server's, loaded into the emptied database.
	imp := &models.Backup{ServerID: srv.ID, Direction: models.DirDatabaseImport, Phase: models.BackupPending,
		DatabaseID: db.ID, Path: "/dump.sql", Wipe: true}
	if err := st.CreateDatabaseImport(imp); err != nil {
		t.Fatalf("create import: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, rec, srv.ID, imp.ID, models.BackupSucceeded, 5*time.Minute)
	if got := sql("SELECT x FROM imported; SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE();"); strings.TrimSpace(got) != "42\n1" {
		t.Fatalf("after the import the database holds %q, want the file's table alone", got)
	}
	// The server could not start while it ran, and can now.
	if err := st.StartServer(srv.ID, time.Now()); err != nil {
		t.Errorf("start after the import: %v", err)
	}
}

// deployMariaDB runs a MariaDB in a namespace of its own, with a database and
// an account holding every privilege on it -- what Quetzal provisions -- and
// returns its pod once it answers.
func deployMariaDB(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, dbName, user, password string) string {
	t.Helper()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	labels := map[string]string{"app": "mariadb"}
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "mariadb",
					Image: mariadbFixtureImage,
					Env: []corev1.EnvVar{
						{Name: "MARIADB_ROOT_PASSWORD", Value: "rootpw"},
						{Name: "MARIADB_DATABASE", Value: dbName},
						{Name: "MARIADB_USER", Value: user},
						{Name: "MARIADB_PASSWORD", Value: password},
					},
					Ports: []corev1.ContainerPort{{ContainerPort: 3306}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: []string{"healthcheck.sh", "--connect", "--innodb_initialized"}},
					}, PeriodSeconds: 3},
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("mariadb: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mariadb", Namespace: ns},
		Spec:       corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 3306, TargetPort: intstr.FromInt32(3306)}}},
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("mariadb svc: %v", err)
	}
	var pod string
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=mariadb"})
		if err != nil || len(pods.Items) == 0 {
			return false, nil
		}
		for _, c := range pods.Items[0].Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				pod = pods.Items[0].Name
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("mariadb not ready: %v\n%s", err, minioDiagnosis(ctx, cs, ns))
	}
	return pod
}

// execIn runs a command in a container of any pod and returns its stdout.
func execIn(ctx context.Context, t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string, cmd []string) string {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true}, scheme.ParameterCodec)
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
