package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// A stopped, crashed or sleeping server answered its stats with a 409, and the
// panel, which polls them every four seconds, put a red error in the browser
// console each time. Having nothing to measure is an answer: 200, available
// false and why, with the limits the server is set to.
func TestStatsOfAServerThatIsNotRunning(t *testing.T) {
	ts, c, st, _, _ := newTestServerFull(t)
	setupAdmin(t, ts.URL, c)
	srv := &models.Server{
		Slug: "idle-a1b2", DisplayName: "idle", Namespace: reconciler.NamespaceFor("idle-a1b2"),
		DesiredState: models.StateStopped, Resources: models.Resources{Memory: "1536Mi", CPU: "1"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	r, err := c.Get(ts.URL + "/api/servers/" + itoa(srv.ID) + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Available   *bool
		Reason      string
		MemoryLimit string
		CPULimit    string `json:"cpuLimit"`
	}
	_ = json.NewDecoder(r.Body).Decode(&got)
	if r.StatusCode != http.StatusOK || got.Available == nil || *got.Available || got.Reason == "" {
		t.Errorf("stats of a stopped server = %d %+v, want 200 with available false and a reason", r.StatusCode, got)
	}
	if got.MemoryLimit != "1536Mi" || got.CPULimit != "1" {
		t.Errorf("limits = %q / %q, want the server's 1536Mi / 1", got.MemoryLimit, got.CPULimit)
	}
}
