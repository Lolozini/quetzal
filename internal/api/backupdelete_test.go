package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

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
