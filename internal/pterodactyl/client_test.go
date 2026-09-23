package pterodactyl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseServerURL(t *testing.T) {
	cases := []struct {
		raw, id, base, wantID string
		bad                   bool
	}{
		{raw: "https://panel.example.com/server/1a2b3c4d", base: "https://panel.example.com", wantID: "1a2b3c4d"},
		{raw: "https://panel.example.com/server/1a2b3c4d/files?x=1", base: "https://panel.example.com", wantID: "1a2b3c4d"},
		{raw: " https://example.com/panel/server/1a2b3c4d ", base: "https://example.com/panel", wantID: "1a2b3c4d"},
		{raw: "https://panel.example.com", id: "1a2b3c4d", base: "https://panel.example.com", wantID: "1a2b3c4d"},
		{raw: "https://panel.example.com/", bad: true},                   // no identifier
		{raw: "panel.example.com/server/1a2b3c4d", bad: true},            // no scheme
		{raw: "ftp://panel.example.com/server/1a2b3c4d", bad: true},      // not http(s)
		{raw: "https://panel.example.com", id: "../../admin", bad: true}, // path injection
		{raw: "https://panel.example.com", id: "1a2b3c4d?x=", bad: true}, // query injection
	}
	for _, c := range cases {
		base, id, err := ParseServerURL(c.raw, c.id)
		if c.bad {
			if err == nil {
				t.Errorf("%q/%q: accepted (%s, %s)", c.raw, c.id, base, id)
			}
			continue
		}
		if err != nil || base != c.base || id != c.wantID {
			t.Errorf("%q/%q = %q, %q, %v; want %q, %q", c.raw, c.id, base, id, err, c.base, c.wantID)
		}
	}
}

const serverJSON = `{"object":"server","attributes":{"identifier":"1a2b3c4d","name":"Survival",
 "docker_image":"ghcr.io/ptero/java:21","invocation":"java -jar server.jar","egg_features":["eula"],
 "is_suspended":false,"is_installing":false,"status":null,
 "limits":{"memory":4096,"swap":0,"disk":20000,"io":500,"cpu":200,"threads":null,"oom_disabled":true},
 "feature_limits":{"databases":0,"allocations":2,"backups":3},
 "relationships":{
  "allocations":{"object":"list","data":[
   {"object":"allocation","attributes":{"id":2,"ip":"1.2.3.4","port":25575,"is_default":false}},
   {"object":"allocation","attributes":{"id":1,"ip":"1.2.3.4","port":25565,"is_default":true}}]},
  "variables":{"object":"list","data":[
   {"object":"egg_variable","attributes":{"env_variable":"SERVER_JARFILE","default_value":"server.jar","server_value":"paper.jar","is_editable":true}},
   {"object":"egg_variable","attributes":{"env_variable":"MINECRAFT_VERSION","default_value":"latest","server_value":null,"is_editable":true}}]},
  "egg":{"object":"egg","attributes":{"uuid":"e1","name":"Paper"}}}}}`

func TestGetServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ptlc_x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/client/servers/1a2b3c4d" || !strings.Contains(r.URL.RawQuery, "egg") {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write([]byte(serverJSON))
	}))
	defer ts.Close()
	c := &Client{Base: ts.URL, Key: "ptlc_x", HTTP: ts.Client()}
	s, err := c.GetServer(context.Background(), "1a2b3c4d")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Survival" || s.EggName != "Paper" || s.MemoryMB != 4096 || s.CPUPercent != 200 || s.DiskMB != 20000 || s.BackupLimit != 3 {
		t.Errorf("server = %+v", s)
	}
	if len(s.Allocations) != 2 || len(s.Variables) != 2 {
		t.Fatalf("allocations/variables = %+v / %+v", s.Allocations, s.Variables)
	}
	// A null server_value falls back to the variable's default.
	if s.Variables[0].Value != "paper.jar" || s.Variables[1].Value != "latest" {
		t.Errorf("variables = %+v", s.Variables)
	}

	c.Key = "ptlc_wrong"
	_, err = c.GetServer(context.Background(), "1a2b3c4d")
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "ptlc_") {
		t.Errorf("wrong key: err = %v", err)
	}
}

func TestErrorDetail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errors":[{"code":"TooManyBackupsException","status":"400","detail":"Backups are disabled for this server."}]}`))
	}))
	defer ts.Close()
	c := &Client{Base: ts.URL, Key: "k", HTTP: ts.Client()}
	_, err := c.CreateBackup(context.Background(), "1a2b3c4d", "x")
	if err == nil || !strings.Contains(err.Error(), "Backups are disabled") {
		t.Errorf("err = %v", err)
	}
}

func TestNotThePanel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>hello</html>"))
	}))
	defer ts.Close()
	c := &Client{Base: ts.URL, Key: "k", HTTP: ts.Client()}
	if _, err := c.GetServer(context.Background(), "1a2b3c4d"); err == nil || !strings.Contains(err.Error(), "panel address") {
		t.Errorf("err = %v", err)
	}
}
