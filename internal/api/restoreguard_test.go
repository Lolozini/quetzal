package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// The recette of 0.10.0 queued a restore on a stopped server and started the
// server straight after: the start was accepted, the restore waited for the
// server to stop again -- days, on a real server -- with the file manager and
// SFTP down all along, could not be cancelled, and then rolled the world back
// over everything played since. A waiting restore now refuses the start, can
// be cancelled, and says so to the file manager at once.
func TestARestoreWaitingRefusesTheStart(t *testing.T) {
	srv, admin, st := newTestServerStore(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "s", "template": "generic-process"})
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatalf("create server: %v", err)
	}
	r.Body.Close()
	base := srv.URL + "/api/servers/" + itoa(created.ID)
	snapshot := func() uint {
		t.Helper()
		b := &models.Backup{ServerID: created.ID, Direction: models.DirBackup, Phase: models.BackupSucceeded}
		if err := st.CreateBackup(b); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
		return b.ID
	}
	answer := func(r *http.Response) (int, string) {
		t.Helper()
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(body)
	}
	start := func() (int, string) {
		return answer(post(t, admin, base+"/power", map[string]string{"action": "start"}))
	}

	first, second := snapshot(), snapshot()
	var restore struct{ ID uint }
	r = post(t, admin, base+"/backups/"+itoa(first)+"/restore", nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&restore)
	r.Body.Close()

	if code, body := start(); code != http.StatusConflict || !strings.Contains(body, "restore") {
		t.Errorf("start under a waiting restore = %d %s, want 409 naming the restore", code, body)
	}
	if got, _ := st.GetServer(created.ID); got.DesiredState == models.StateRunning {
		t.Fatal("the server was started under a waiting restore")
	}
	// A second restore would only replace the first, whichever ended last.
	if code, _ := answer(post(t, admin, base+"/backups/"+itoa(second)+"/restore", nil)); code != http.StatusConflict {
		t.Errorf("second restore = %d, want 409", code)
	}
	// The data manager is down for the restore: the file manager says so at
	// once, where it waited two minutes for the pod.
	began := time.Now()
	resp, err := admin.Get(base + "/files?path=/")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := answer(resp); code != http.StatusConflict || !strings.Contains(body, "restore") {
		t.Errorf("files under a waiting restore = %d %s, want 409 naming the restore", code, body)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("the file manager answered after %s", waited)
	}

	// Cancelled, it is gone, and the server starts.
	req, _ := http.NewRequest(http.MethodDelete, base+"/backups/"+itoa(restore.ID), nil)
	resp, err = admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := answer(resp); code != http.StatusNoContent {
		t.Fatalf("cancel the waiting restore = %d %s, want 204", code, body)
	}
	if code, body := start(); code != http.StatusOK {
		t.Fatalf("start once the restore is cancelled = %d %s", code, body)
	}
	// And a running server is not restored.
	if code, _ := answer(post(t, admin, base+"/backups/"+itoa(second)+"/restore", nil)); code != http.StatusConflict {
		t.Errorf("restore of a running server = %d, want 409", code)
	}
}
