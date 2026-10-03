package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Every error of the API is JSON but the ones no route took: "404 page not
// found" in plain text (recette of 0.10.0, R-30). Those are JSON too now, and
// a known route called with another method still says 405, and which it takes.
func TestARouteNobodyTakesAnswersInJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	check := func(method, path string, want int) http.Header {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Error string }
		if resp.StatusCode != want || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") ||
			json.NewDecoder(resp.Body).Decode(&body) != nil || body.Error == "" {
			t.Errorf("%s %s = %d %s, want %d with a JSON error", method, path, resp.StatusCode, resp.Header.Get("Content-Type"), want)
		}
		return resp.Header
	}
	check(http.MethodGet, "/api/nonexistent", http.StatusNotFound)
	if allow := check(http.MethodPut, "/api/version", http.StatusMethodNotAllowed).Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want the methods the route takes", allow)
	}
}
