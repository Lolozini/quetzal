package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// Deleting a backup must respect what the record still owns. An operation in
// flight holds a Job — and a restore also holds the exclusive write mount on the
// data volume, which the reconciler only keeps clear while the row exists — so
// dropping it would corrupt the volume. A succeeded backup owns a restic
// snapshot, so it goes through the Deleting phase instead of vanishing while its
// data stays in the bucket.
func TestDeleteBackupRespectsInFlightAndSnapshots(t *testing.T) {
	srv, admin, st := newTestServerStore(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process"})
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("create server: %v", err)
	}
	base := srv.URL + "/api/servers/" + itoa(created.ID) + "/backups/"

	seed := func(d models.BackupDirection, p models.BackupPhase) *models.Backup {
		t.Helper()
		b := &models.Backup{ServerID: created.ID, Direction: d, Phase: p}
		if err := st.CreateBackup(b); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
		return b
	}
	del := func(id uint) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, base+itoa(id), nil)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A running restore must not be deletable: the reconciler keeps the data
	// manager scaled down only while this row exists.
	running := seed(models.DirRestore, models.BackupRunning)
	if got := del(running.ID); got != http.StatusConflict {
		t.Errorf("delete running restore = %d, want 409", got)
	}
	if b, err := st.GetBackup(running.ID); err != nil || b.Phase != models.BackupRunning {
		t.Errorf("running restore was disturbed: %+v (err %v)", b, err)
	}
	if got := del(seed(models.DirBackup, models.BackupPending).ID); got != http.StatusConflict {
		t.Errorf("delete pending backup = %d, want 409", got)
	}

	// A succeeded backup owns a snapshot: the row survives as Deleting until the
	// controller has forgotten it from the repository.
	done := seed(models.DirBackup, models.BackupSucceeded)
	if got := del(done.ID); got != http.StatusAccepted {
		t.Errorf("delete succeeded backup = %d, want 202", got)
	}
	b, err := st.GetBackup(done.ID)
	if err != nil {
		t.Fatalf("succeeded backup disappeared before its snapshot was forgotten: %v", err)
	}
	if b.Phase != models.BackupDeleting {
		t.Errorf("phase = %q, want Deleting", b.Phase)
	}

	// Nothing to forget for a failed operation: it goes straight away.
	failed := seed(models.DirBackup, models.BackupFailed)
	if got := del(failed.ID); got != http.StatusNoContent {
		t.Errorf("delete failed backup = %d, want 204", got)
	}
	if _, err := st.GetBackup(failed.ID); err == nil {
		t.Error("failed backup record still present")
	}
}

// Deleting a server has to take its snapshots with it. The backup rows are
// dropped a moment later, so anything left in the bucket is unreachable from the
// panel from then on — invisible, unrestorable, and still billed. The purge Job
// therefore has to be created before the rows go, and in the control plane's
// namespace, because the server's is about to be torn down.
func TestDeleteServerPurgesItsSnapshots(t *testing.T) {
	srv, admin, st, apiSrv, cs := newTestServerFull(t)
	apiSrv.Namespace = "quetzal"
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	if r := put(t, admin, srv.URL+"/api/backup-config", map[string]any{
		"endpoint": "s3.example", "bucket": "b", "prefix": "p", "region": "r", "useSSL": true,
		"keepLast": 3, "accessKey": "ak", "secretKey": "sk", "repoPassword": "rp",
	}); r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK {
		t.Fatalf("configure backups = %d", r.StatusCode)
	}

	newServer := func(name string) (uint, string) {
		t.Helper()
		var created struct {
			ID   uint
			Slug string
		}
		r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": name, "template": "generic-process"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create server = %d", r.StatusCode)
		}
		json.NewDecoder(r.Body).Decode(&created)
		r.Body.Close()
		return created.ID, created.Slug
	}
	deleteServer := func(id uint) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/servers/"+itoa(id), nil)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("delete server: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete server = %d, want 204", resp.StatusCode)
		}
	}
	purgeJobs := func() []string {
		t.Helper()
		jobs, err := cs.BatchV1().Jobs("quetzal").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		var names []string
		for _, j := range jobs.Items {
			names = append(names, j.Name)
		}
		return names
	}

	// A server that completed a backup owns a snapshot: deleting it must purge.
	withSnapshot, slug := newServer("has-backup")
	if err := st.CreateBackup(&models.Backup{
		ServerID: withSnapshot, Direction: models.DirBackup, Phase: models.BackupSucceeded,
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	deleteServer(withSnapshot)
	want := "quetzal-purge-" + slug
	if got := purgeJobs(); len(got) != 1 || got[0] != want {
		t.Fatalf("purge jobs = %v, want [%s]", got, want)
	}
	// The credentials it needs must be there too, under a name of its own.
	if _, err := cs.CoreV1().Secrets("quetzal").Get(context.Background(),
		"quetzal-backup-creds-"+slug, metav1.GetOptions{}); err != nil {
		t.Errorf("purge Job has no credentials: %v", err)
	}

	// A server that never completed one has no repository, so nothing to purge.
	noSnapshot, _ := newServer("no-backup")
	if err := st.CreateBackup(&models.Backup{
		ServerID: noSnapshot, Direction: models.DirBackup, Phase: models.BackupFailed,
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	deleteServer(noSnapshot)
	if got := purgeJobs(); len(got) != 1 {
		t.Errorf("a server with no completed backup was purged anyway: %v", got)
	}
}
