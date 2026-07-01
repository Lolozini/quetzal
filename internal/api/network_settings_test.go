package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/store"
)

// put issues an authenticated PUT with a JSON body and returns the response.
func put(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(http.MethodPut, url, &buf)
	if err != nil {
		t.Fatalf("new PUT %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func TestNetworkSettingsRoundTrip(t *testing.T) {
	ts, c, st := newTestServerStore(t)
	post(t, c, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"})

	// Defaults: no endpoint host configured.
	var got struct {
		EndpointHost string `json:"endpointHost"`
		NodeAddress  string `json:"nodeAddress"`
	}
	getJSON(t, c, ts.URL+"/api/network-settings", &got)
	if got.EndpointHost != "" {
		t.Fatalf("endpointHost = %q, want empty by default", got.EndpointHost)
	}

	// Setting a host is trimmed and persisted (readable back via API and store).
	if r := put(t, c, ts.URL+"/api/network-settings", map[string]string{"endpointHost": "  play.example.com  "}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", r.StatusCode)
	}
	getJSON(t, c, ts.URL+"/api/network-settings", &got)
	if got.EndpointHost != "play.example.com" {
		t.Fatalf("endpointHost = %q, want play.example.com", got.EndpointHost)
	}
	if v, _ := st.GetSetting(store.SettingEndpointHost); v != "play.example.com" {
		t.Fatalf("stored setting = %q, want play.example.com", v)
	}

	// Blank clears it.
	if r := put(t, c, ts.URL+"/api/network-settings", map[string]string{"endpointHost": ""}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("clear PUT status = %d, want 204", r.StatusCode)
	}
	getJSON(t, c, ts.URL+"/api/network-settings", &got)
	if got.EndpointHost != "" {
		t.Fatalf("endpointHost = %q after clear, want empty", got.EndpointHost)
	}
}

func TestNetworkSettingsRequiresAdmin(t *testing.T) {
	ts, _, _ := newTestServerStore(t)
	// No session: the settings endpoint must not be reachable.
	noauth := &http.Client{}
	r, err := noauth.Get(ts.URL + "/api/network-settings")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated GET returned 200, want auth failure")
	}
}
