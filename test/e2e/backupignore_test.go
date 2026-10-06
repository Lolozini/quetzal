//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/backup"
	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EBackupIgnore backs up a server whose .quetzalignore lists its game
// and its logs, then restores it: the snapshot holds neither, and the restore
// puts the rest back while it leaves them as they are -- the game a newer
// version by then, where a restore that matched the volume to the snapshot
// would have deleted it.
func TestE2EBackupIgnore(t *testing.T) {
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
	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-ignore", DisplayName: "ignore", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-ignore"),
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
	// Four megabytes of game, a save, a log, and the list.
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", `set -e
mkdir -p /data/game /data/saves /data/logs
head -c 4000000 /dev/zero > /data/game/big.pak
echo game-v1 > /data/game/version.txt
echo save-v1 > /data/saves/world.sav
echo a > /data/logs/a.log
printf '# the game, downloaded again at will\n/game/\n*.log\n' > /data/.quetzalignore`})

	mgr := backup.NewManager(st, cluster.New(st, cluster.Clients{Clientset: cs, Config: cfg}))
	b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(b); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	waitBackupPhase(ctx, t, st, mgr, rec, srv.ID, b.ID, models.BackupSucceeded, 4*time.Minute)
	done, _ := st.GetBackup(b.ID)
	if !strings.Contains(done.Ignored, "/game/") || done.Message != "" {
		t.Fatalf("backup ignored %q, message %q", done.Ignored, done.Message)
	}
	if done.SizeBytes <= 0 || done.SizeBytes > 1000000 {
		t.Errorf("backup size = %d: the game's four megabytes should not be in it", done.SizeBytes)
	}

	// Since the backup: the game updated, the save moved on, logs turned over.
	execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", `set -e
echo game-v2 > /data/game/version.txt
echo save-v2 > /data/saves/world.sav
echo new > /data/saves/new.sav
rm /data/logs/a.log
echo b > /data/logs/b.log`})

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
	waitBackupPhase(ctx, t, st, mgr, rec, srv.ID, r.ID, models.BackupSucceeded, 4*time.Minute)
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatalf("start: %v", err)
	}
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)
	pod, err = console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod after restore: %v", err)
	}
	out := execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", `cd /data
for f in game/version.txt game/big.pak saves/world.sav saves/new.sav logs/a.log logs/b.log .quetzalignore; do
  if [ -f "$f" ]; then printf '%s=%s\n' "$f" "$(head -n 1 "$f" | tr -d '\0')"; else echo "$f=ABSENT"; fi
done`})
	lines := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{
		"game/version.txt=game-v2", // left as it is
		"game/big.pak=",            // still there: zeros, read as nothing
		"saves/world.sav=save-v1",  // the backup's
		"saves/new.sav=ABSENT",     // made after the backup
		"logs/a.log=ABSENT",        // never in the snapshot
		"logs/b.log=b",             // left as it is
		".quetzalignore=# the game, downloaded again at will",
	} {
		if !lines[want] {
			t.Errorf("want %q; files after the restore:\n%s", want, out)
		}
	}
}
