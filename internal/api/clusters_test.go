package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// a syntactically valid kubeconfig pointing at a closed port: it parses (so the
// cluster can be registered) but is unreachable (so probes fail fast), letting
// us exercise the cluster API without a real remote cluster.
const fakeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: edge
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: edge
  context:
    cluster: edge
    user: edge
current-context: edge
users:
- name: edge
  user:
    token: testtoken
`

// TestClusterDefaultStorageClass verifies the per-cluster default storageClass is
// admin-controlled and applied to servers created on that cluster, rather than
// being chosen per server by tenants.
func TestClusterDefaultStorageClass(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	var edge struct {
		ID                  uint   `json:"id"`
		DefaultStorageClass string `json:"defaultStorageClass"`
	}
	r := post(t, admin, srv.URL+"/api/clusters", map[string]string{"name": "edge", "kubeconfig": fakeKubeconfig})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create cluster = %d, want 201", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&edge)

	// Admin pins the cluster's default storageClass.
	body, _ := json.Marshal(map[string]any{"defaultStorageClass": "fast-ssd"})
	pr, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/clusters/"+itoa(edge.ID), bytes.NewReader(body))
	pr.Header.Set("Content-Type", "application/json")
	presp, err := admin.Do(pr)
	if err != nil || presp.StatusCode != http.StatusOK {
		t.Fatalf("patch cluster sc = %v / %d, want 200", err, presp.StatusCode)
	}
	var updated struct {
		DefaultStorageClass string `json:"defaultStorageClass"`
	}
	json.NewDecoder(presp.Body).Decode(&updated)
	if updated.DefaultStorageClass != "fast-ssd" {
		t.Fatalf("defaultStorageClass = %q, want fast-ssd", updated.DefaultStorageClass)
	}

	// A server created on the cluster inherits it, even if the client tries to set
	// its own (storageClass is admin-controlled, not tenant-chosen).
	var created struct {
		Storage struct {
			StorageClass string `json:"storageClass"`
		} `json:"storage"`
	}
	cr := post(t, admin, srv.URL+"/api/servers", map[string]any{
		"name": "edge srv", "template": "generic-process", "cluster": "edge",
		"storage": map[string]any{"type": "pvc", "storageClass": "tenant-tried-this"},
	})
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create server = %d, want 201", cr.StatusCode)
	}
	json.NewDecoder(cr.Body).Decode(&created)
	if created.Storage.StorageClass != "fast-ssd" {
		t.Errorf("server storageClass = %q, want fast-ssd (cluster default, not tenant input)", created.Storage.StorageClass)
	}
}

func TestClusters(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})
	createUser(t, admin, srv.URL, map[string]any{"username": "alice", "password": "alicepw12"})
	alice := loginAs(t, srv.URL, "alice", "alicepw12")

	// Non-admin cannot register a cluster.
	if r := post(t, alice, srv.URL+"/api/clusters", map[string]string{"name": "x", "kubeconfig": fakeKubeconfig}); r.StatusCode != http.StatusForbidden {
		t.Errorf("alice create cluster = %d, want 403", r.StatusCode)
	}
	// Invalid kubeconfig is rejected.
	if r := post(t, admin, srv.URL+"/api/clusters", map[string]string{"name": "bad", "kubeconfig": "not a kubeconfig"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid kubeconfig = %d, want 400", r.StatusCode)
	}
	// Register a (registerable but unreachable) cluster.
	var edge struct {
		ID        uint   `json:"id"`
		Slug      string `json:"slug"`
		Reachable bool   `json:"reachable"`
	}
	r := post(t, admin, srv.URL+"/api/clusters", map[string]string{"name": "edge", "kubeconfig": fakeKubeconfig})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create cluster = %d, want 201", r.StatusCode)
	}
	json.NewDecoder(r.Body).Decode(&edge)
	if edge.Slug != "edge" || edge.ID == 0 {
		t.Fatalf("created cluster = %+v", edge)
	}
	// Duplicate name -> conflict.
	if r := post(t, admin, srv.URL+"/api/clusters", map[string]string{"name": "edge", "kubeconfig": fakeKubeconfig}); r.StatusCode != http.StatusConflict {
		t.Errorf("duplicate cluster = %d, want 409", r.StatusCode)
	}
	// Any authenticated user can list clusters (to pick a deploy target), but a
	// non-admin must not see probe details (which can leak internal addresses).
	var list []map[string]any
	getJSON(t, alice, srv.URL+"/api/clusters", &list)
	if len(list) == 0 {
		t.Fatalf("alice should see the cluster list")
	}
	for _, c := range list {
		if c["slug"] == "edge" {
			if _, ok := c["statusMessage"]; ok {
				t.Errorf("non-admin should not see cluster statusMessage")
			}
			if _, ok := c["version"]; ok {
				t.Errorf("non-admin should not see cluster version")
			}
		}
	}

	// Create a server targeting the registered cluster.
	var created struct {
		ID        uint `json:"id"`
		ClusterID uint `json:"clusterId"`
	}
	cr := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "edge srv", "template": "generic-process", "cluster": "edge"})
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create server on edge = %d, want 201", cr.StatusCode)
	}
	json.NewDecoder(cr.Body).Decode(&created)
	if created.ClusterID != edge.ID {
		t.Errorf("server clusterId = %d, want %d", created.ClusterID, edge.ID)
	}
	// Unknown cluster -> 400.
	if r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "nope", "template": "generic-process", "cluster": "ghost"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown cluster = %d, want 400", r.StatusCode)
	}

	// Cluster with servers cannot be deleted.
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/clusters/"+itoa(edge.ID), nil)
	if dr, _ := admin.Do(delReq); dr.StatusCode != http.StatusConflict {
		t.Errorf("delete cluster with servers = %d, want 409", dr.StatusCode)
	}
	// Remove the server, then the cluster can be deleted.
	srvDel, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/servers/"+itoa(created.ID), nil)
	if dr, _ := admin.Do(srvDel); dr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete server = %d, want 204", dr.StatusCode)
	}
	delReq2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/clusters/"+itoa(edge.ID), nil)
	if dr, _ := admin.Do(delReq2); dr.StatusCode != http.StatusNoContent {
		t.Errorf("delete empty cluster = %d, want 204", dr.StatusCode)
	}

	// A default-cluster server create materializes the local cluster row.
	if r := post(t, admin, srv.URL+"/api/servers", map[string]any{"name": "local srv", "template": "generic-process"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("default-cluster create = %d, want 201", r.StatusCode)
	}

	// The local (in-cluster) cluster is protected from deletion.
	var clusters []struct {
		ID        uint `json:"id"`
		InCluster bool `json:"inCluster"`
	}
	getJSON(t, admin, srv.URL+"/api/clusters", &clusters)
	var localID uint
	for _, c := range clusters {
		if c.InCluster {
			localID = c.ID
		}
	}
	if localID == 0 {
		t.Fatal("local cluster should exist after a default-cluster server create")
	}
	localDel, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/clusters/"+itoa(localID), nil)
	if dr, _ := admin.Do(localDel); dr.StatusCode != http.StatusBadRequest {
		t.Errorf("delete local cluster = %d, want 400", dr.StatusCode)
	}
}
