package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// These tests execute the generated shell scripts against a real local restic
// repository. QUETZAL_TEST_RESTIC names the binary, not a replacement command.
func realRestic(t *testing.T) (run func(string) error, snapshots func() [][]string) {
	t.Helper()
	binary := os.Getenv("QUETZAL_TEST_RESTIC")
	if binary == "" {
		t.Skip("set QUETZAL_TEST_RESTIC to a restic binary")
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	data := filepath.Join(root, "data")
	dumps := filepath.Join(root, "dumps")
	for _, dir := range []string{bin, data, dumps} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(binary, filepath.Join(bin, "restic")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{data, dumps} {
		if err := os.WriteFile(filepath.Join(dir, "sample"), []byte("durable data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "RESTIC_REPOSITORY="+filepath.Join(root, "repo"), "RESTIC_PASSWORD=test-password", "RESTIC_CACHE_DIR="+filepath.Join(root, "cache"))
	run = func(script string) error {
		script = strings.ReplaceAll(script, mountPath, data)
		script = strings.ReplaceAll(script, dumpsPath, dumps)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("restic script: %v\n%s", err, out)
		}
		return err
	}
	snapshots = func() [][]string {
		cmd := exec.Command(binary, "snapshots", "--json")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var all []struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(out, &all); err != nil {
			t.Fatal(err)
		}
		var tags [][]string
		for _, snapshot := range all {
			tags = append(tags, snapshot.Tags)
		}
		return tags
	}
	return run, snapshots
}

func TestResticRetryDoesNotDuplicateSnapshot(t *testing.T) {
	run, snapshots := realRestic(t)
	p := Params{Slug: "retry", BackupID: 1, Direction: models.DirBackup, KeepLast: 2}
	script := BuildJob(p).Spec.Template.Spec.Containers[0].Command[2]
	if err := run(script); err != nil {
		t.Fatal(err)
	}
	// A completed snapshot can outlive its pod: retrying the same operation
	// after losing that pod must not occupy a second retention slot.
	if err := run(script); err != nil {
		t.Fatal(err)
	}
	if got := snapshots(); len(got) != 1 {
		t.Fatalf("retry created duplicate recovery points: %v", got)
	}
}

func TestResticRetentionTracksChangingDatabasePaths(t *testing.T) {
	run, snapshots := realRestic(t)
	m, st, srv, _, cs := dbFixture(t, true)
	cfg, _ := st.GetBackupConfig()
	cfg.KeepLast = 1
	if err := st.SaveBackupConfig(cfg, "", "", ""); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for round := range 2 {
		b := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		m.processPending(ctx)
		b, _ = st.GetBackup(b.ID)
		p := Params{Slug: srv.Slug, BackupID: b.ID, Direction: models.DirBackup, KeepLast: 1}
		if round == 1 {
			p.Databases = []DatabaseParams{{Name: "game"}}
		}
		if err := run(BuildJob(p).Spec.Template.Spec.Containers[0].Command[2]); err != nil {
			t.Fatal(err)
		}
		finishJob(t, cs, b.JobName, "", false)
		m.processRunning(ctx)
		m.processDeleting(ctx)
		deleting, err := st.ListBackupsByPhase(models.BackupDeleting)
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range deleting {
			job, err := cs.BatchV1().Jobs(dbNS).Get(ctx, old.JobName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := run(job.Spec.Template.Spec.Containers[0].Command[2]); err != nil {
				t.Fatal(err)
			}
			finishJob(t, cs, old.JobName, "", false)
		}
		m.processDeleting(ctx)
	}
	if got := snapshots(); len(got) != 1 {
		t.Fatalf("path groups retained untracked snapshots: %v", got)
	}
	rows, err := st.ListBackupsForServer(srv.ID)
	if err != nil || len(rows) != 1 || rows[0].Phase != models.BackupSucceeded {
		t.Fatalf("recovery history differs from repository: %+v %v", rows, err)
	}
}
