package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// seedTransferEnv creates an admin + a server and returns its transfer URL,
// plus the store so the test can seed a target cluster / backup config.
func seedTransferEnv(t *testing.T) (string, *http.Client, *store.Store, string) {
	t.Helper()
	srv, admin, st := newTestServerStore(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	var created struct{ ID uint }
	r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "mover", "template": "generic-process"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&created)
	return srv.URL, admin, st, srv.URL + "/api/servers/" + itoa(created.ID)
}

func configureBackups(t *testing.T, st *store.Store) {
	t.Helper()
	cfg := &models.BackupConfig{Endpoint: "minio:9000", Bucket: "quetzal", KeepLast: 7}
	if err := st.SaveBackupConfig(cfg, "ak", "sk", "pw"); err != nil {
		t.Fatalf("save backup config: %v", err)
	}
}

func seedCluster(t *testing.T, st *store.Store) uint {
	t.Helper()
	c := &models.Cluster{Slug: "c2", Name: "Cluster 2"}
	if err := st.CreateCluster(c, ""); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	return c.ID
}

func TestTransferRequiresAdmin(t *testing.T) {
	base, admin, _, _ := seedTransferEnv(t)
	createUser(t, admin, base, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, base, "alice", "alicepw12")
	// Alice owns her own server but isn't a server-admin.
	var created struct{ ID uint }
	r := post(t, alice, base+"/api/servers", map[string]any{"name": "a", "template": "generic-process"})
	json.NewDecoder(r.Body).Decode(&created)
	if rr := post(t, alice, base+"/api/servers/"+itoa(created.ID)+"/transfer", map[string]any{"targetCluster": 2}); rr.StatusCode != http.StatusForbidden {
		t.Errorf("owner transfer = %d, want 403 (admin-only)", rr.StatusCode)
	}
}

func TestTransferValidation(t *testing.T) {
	_, admin, st, url := seedTransferEnv(t)

	// No backup target configured yet → 400 even with a valid cluster.
	cid := seedCluster(t, st)
	if r := post(t, admin, url+"/transfer", map[string]any{"targetCluster": cid}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("transfer without backups = %d, want 400", r.StatusCode)
	}

	configureBackups(t, st)
	// Same cluster (server is on local = 0) → 400.
	if r := post(t, admin, url+"/transfer", map[string]any{"targetCluster": 0}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("transfer to same cluster = %d, want 400", r.StatusCode)
	}
	// Unknown cluster → 400.
	if r := post(t, admin, url+"/transfer", map[string]any{"targetCluster": 9999}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("transfer to unknown cluster = %d, want 400", r.StatusCode)
	}
}

func TestTransferStartsAndBlocksActions(t *testing.T) {
	_, admin, st, url := seedTransferEnv(t)
	cid := seedCluster(t, st)
	configureBackups(t, st)

	r := post(t, admin, url+"/transfer", map[string]any{"targetCluster": cid})
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer start = %d, want 202", r.StatusCode)
	}
	// The server is now stopped with a transfer recorded.
	var srv models.Server
	getJSON(t, admin, url, &srv)
	if srv.Transfer == nil || srv.Transfer.TargetCluster != cid {
		t.Fatalf("transfer not recorded: %+v", srv.Transfer)
	}
	if srv.DesiredState != models.StateStopped {
		t.Errorf("desired = %q, want Stopped during transfer", srv.DesiredState)
	}
	// Power and a second transfer are blocked while it's in progress.
	if pr := post(t, admin, url+"/power", map[string]any{"action": "start"}); pr.StatusCode != http.StatusConflict {
		t.Errorf("power during transfer = %d, want 409", pr.StatusCode)
	}
	if tr := post(t, admin, url+"/transfer", map[string]any{"targetCluster": cid}); tr.StatusCode != http.StatusConflict {
		t.Errorf("second transfer = %d, want 409", tr.StatusCode)
	}
}
