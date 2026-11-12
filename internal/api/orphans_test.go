package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// The controller's report of namespaces without a server row reaches the panel
// as a list an administrator reads: it is how a restored database is noticed
// before the volumes go. Anyone else is turned away, and a cluster that has been
// removed since leaves no banner behind.
func TestOrphanNamespacesAreListedForAdministratorsOnly(t *testing.T) {
	ts, admin, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, admin)
	createUser(t, admin, ts.URL, map[string]any{"username": "alice", "password": "alicepass1"})
	alice := loginAs(t, ts.URL, "alice", "alicepass1")

	var none []map[string]any
	getJSON(t, admin, ts.URL+"/api/orphan-namespaces", &none)
	if len(none) != 0 {
		t.Fatalf("a clean install lists %v", none)
	}

	c := &models.Cluster{Slug: "home", Name: "Home"}
	if err := st.CreateCluster(c, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReportOrphans(models.OrphanReport{
		ClusterID: c.ID, Since: time.Now(), Names: []string{"quetzal-srv-a", "quetzal-srv-b"},
		Managed: 9, Refused: false, GraceSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	// A report of a cluster that no longer exists.
	if _, err := st.ReportOrphans(models.OrphanReport{ClusterID: 999, Names: []string{"quetzal-srv-gone"}, Managed: 1}); err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Cluster      string   `json:"cluster"`
		Names        []string `json:"names"`
		Managed      int      `json:"managed"`
		Refused      bool     `json:"refused"`
		GraceSeconds int64    `json:"graceSeconds"`
	}
	getJSON(t, admin, ts.URL+"/api/orphan-namespaces", &got)
	if len(got) != 1 || got[0].Cluster != "Home" || len(got[0].Names) != 2 || got[0].Managed != 9 || got[0].GraceSeconds != 3600 {
		t.Fatalf("listed %+v, want the one report of cluster Home with two names", got)
	}

	resp, err := alice.Get(ts.URL + "/api/orphan-namespaces")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a user without admin rights got %d, want 403", resp.StatusCode)
	}
}
