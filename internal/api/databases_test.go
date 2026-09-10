package api_test

import (
	"net/http"
	"testing"
)

// The namespace of a managed host is Quetzal's to choose. It creates that
// namespace and deletes it when the host goes, so a name pointing at one that
// already exists would put a workload in someone else's namespace and then
// collect it. The endpoint is reachable by an admin scoped to database hosts
// alone, which makes this an escalation out of a delegated role.
func TestManagedDatabaseHostCannotClaimAForeignNamespace(t *testing.T) {
	srv, admin := newTestServer(t)
	post(t, admin, srv.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	create := func(ns string) *http.Response {
		t.Helper()
		return post(t, admin, srv.URL+"/api/database-hosts", map[string]any{
			"name": "db-" + ns, "kind": "managed", "clusterId": 1,
			"namespace": ns, "storageSize": "1Gi",
		})
	}
	for _, ns := range []string{"kube-system", "default", "quetzal", "quetzal-srv-victim", "../kube-system"} {
		if r := create(ns); r.StatusCode != http.StatusBadRequest {
			t.Errorf("namespace %q accepted: %d", ns, r.StatusCode)
		}
	}
	// A name Quetzal could have chosen itself is still the operator's to pick,
	// and so is leaving it out.
	for _, ns := range []string{"quetzal-db-analytics", ""} {
		if r := create(ns); r.StatusCode != http.StatusCreated {
			t.Errorf("namespace %q refused: %d", ns, r.StatusCode)
		}
	}
}
