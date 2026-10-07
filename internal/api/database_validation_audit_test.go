package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCPUQuotaRejectsZeroOnCreateAndUpdate(t *testing.T) {
	for _, quota := range []int{0, 1000} {
		t.Run(itoa(uint(quota)), func(t *testing.T) {
			ts, admin, st := newTestServerStore(t)
			post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}).Body.Close()
			createUser(t, admin, ts.URL, map[string]any{"username": "player", "password": "playerpw12", "maxServers": -1, "maxCpuMilli": quota})
			player := loginAs(t, ts.URL, "player", "playerpw12")
			owner, err := st.GetUserByUsername("player")
			if err != nil {
				t.Fatal(err)
			}
			server := &models.Server{Slug: "cpu-audit", Namespace: "quetzal-srv-cpu-audit", OwnerID: owner.ID, Resources: models.Resources{Memory: "512Mi", CPU: "100m"}}
			if err := st.CreateServer(server); err != nil {
				t.Fatal(err)
			}
			for _, zero := range []string{"0", "0m", "0.0", "0e3"} {
				r := post(t, player, ts.URL+"/api/servers", map[string]any{"name": "zero", "template": "generic-process", "memory": "512Mi", "cpu": zero})
				r.Body.Close()
				if r.StatusCode != http.StatusForbidden {
					t.Errorf("create CPU %q quota %d = %d, want 403", zero, quota, r.StatusCode)
				}
				r = doPatch(t, player, ts.URL+"/api/servers/"+itoa(server.ID), map[string]any{"resources": map[string]string{"memory": "512Mi", "cpu": zero}})
				r.Body.Close()
				if r.StatusCode != http.StatusForbidden {
					t.Errorf("update CPU %q quota %d = %d, want 403", zero, quota, r.StatusCode)
				}
			}
		})
	}
}

func TestServerStorageWhitespacePersistsParseableQuantity(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}).Body.Close()
	r := post(t, admin, ts.URL+"/api/servers", map[string]any{"name": "space", "template": "generic-process", "storage": map[string]string{"type": "pvc", "size": " 1Gi "}})
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	var created models.Server
	if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetServer(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resource.ParseQuantity(stored.Storage.Size); err != nil {
		t.Fatalf("persisted storage %q cannot reach builder: %v", stored.Storage.Size, err)
	}
}

func TestManagedDatabasePatchValidatesAndPreservesStorage(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}).Body.Close()
	h := &models.DatabaseHost{Name: "managed", Kind: models.DBHostManaged, StorageSize: "5Gi"}
	if err := st.CreateDatabaseHost(h, "password"); err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/database-hosts/" + itoa(h.ID)
	for _, bad := range []string{"not-a-size", "0", "-1Gi", "10 GB"} {
		r := doPatch(t, admin, url, map[string]string{"storageSize": bad})
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH size %q = %d, want 400", bad, r.StatusCode)
		}
		stored, err := st.GetDatabaseHost(h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.StorageSize != "5Gi" {
			t.Errorf("invalid patch persisted %q", stored.StorageSize)
		}
	}
	r := doPatch(t, admin, url, map[string]string{"storageSize": " 2Gi "})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("valid PATCH = %d", r.StatusCode)
	}
	r = doPatch(t, admin, url, map[string]string{"name": "renamed"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("name PATCH = %d", r.StatusCode)
	}
	stored, err := st.GetDatabaseHost(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StorageSize != "2Gi" {
		t.Errorf("omitted size changed to %q", stored.StorageSize)
	}
}

func TestManagedDatabaseHostNamespaceConflict(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	post(t, admin, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}).Body.Close()
	r := post(t, admin, ts.URL+"/api/database-hosts", map[string]any{"name": "first", "kind": "managed"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("first host = %d", r.StatusCode)
	}
	var host models.DatabaseHost
	if err := json.NewDecoder(r.Body).Decode(&host); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	r = post(t, admin, ts.URL+"/api/database-hosts", map[string]any{"name": "duplicate", "kind": "managed", "namespace": host.Namespace, "clusterId": 99})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Errorf("duplicate namespace = %d, want 409", r.StatusCode)
	}
	hosts, err := st.ListDatabaseHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Errorf("duplicate host persisted: %d hosts", len(hosts))
	}
}
