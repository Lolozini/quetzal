package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestVersionEndpointPublic verifies the build-info endpoint is reachable
// without authentication and reports the (unstamped, in tests) version.
func TestVersionEndpointPublic(t *testing.T) {
	ts, c := newTestServer(t)

	resp, err := c.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must be public)", resp.StatusCode)
	}
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"date"`
		Go      string `json:"go"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Version == "" || info.Go == "" {
		t.Errorf("incomplete version info: %+v", info)
	}
}
