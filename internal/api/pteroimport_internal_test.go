package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// fakePanel is a Pterodactyl client API with one server, 1a2b3c4d.
type fakePanel struct {
	t              *testing.T
	ts             *httptest.Server
	archive        []byte
	backupsRefused bool
	downloadFails  bool

	mu        sync.Mutex
	polls     int
	deleted   []string // backups deleted
	removed   []string // root files deleted
	compress  []string // root entries compressed
	backupsOK int
}

func newFakePanel(t *testing.T) *fakePanel {
	p := &fakePanel{t: t, archive: tarGz(t, map[string]string{"server.jar": "jar", "world/level.dat": "lvl"})}
	p.ts = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.ts.Close)
	return p
}

const fakeServerJSON = `{"object":"server","attributes":{"identifier":"1a2b3c4d","name":"Survival",
 "docker_image":"ghcr.io/example/java:21","egg_features":["eula"],"status":null,
 "limits":{"memory":3072,"disk":0,"cpu":150},"feature_limits":{"backups":1},
 "relationships":{
  "allocations":{"data":[{"attributes":{"port":25575,"is_default":false}},{"attributes":{"port":25565,"is_default":true}}]},
  "variables":{"data":[
   {"attributes":{"env_variable":"VERSION","default_value":"latest","server_value":"1.21.1","is_editable":true}},
   {"attributes":{"env_variable":"MODE","default_value":"a","server_value":"zzz","is_editable":true}},
   {"attributes":{"env_variable":"FIXED","default_value":"x","server_value":"y","is_editable":false}},
   {"attributes":{"env_variable":"EXTRA","default_value":"","server_value":"1","is_editable":true}}]},
  "egg":{"attributes":{"name":"Test Egg"}}}}}`

func (p *fakePanel) serve(w http.ResponseWriter, r *http.Request) {
	const base = "/api/client/servers/1a2b3c4d"
	if strings.HasPrefix(r.URL.Path, "/dl/") {
		if p.downloadFails {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.Write(p.archive)
		return
	}
	if r.Header.Get("Authorization") != "Bearer ptlc_good" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == http.MethodGet && path == "":
		fmt.Fprint(w, fakeServerJSON)
	case r.Method == http.MethodGet && path == "/resources":
		fmt.Fprint(w, `{"attributes":{"resources":{"disk_bytes":3221225472}}}`)
	case r.Method == http.MethodPost && path == "/backups":
		if p.backupsRefused {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"detail":"Backups are disabled for this server."}]}`)
			return
		}
		fmt.Fprint(w, `{"attributes":{"uuid":"b-1","is_successful":false,"completed_at":null}}`)
	case r.Method == http.MethodGet && path == "/backups/b-1":
		p.polls++
		if p.polls < 2 {
			fmt.Fprint(w, `{"attributes":{"uuid":"b-1","is_successful":false,"completed_at":null}}`)
			return
		}
		fmt.Fprint(w, `{"attributes":{"uuid":"b-1","is_successful":true,"completed_at":"2026-09-24T10:00:00Z"}}`)
	case r.Method == http.MethodGet && path == "/backups/b-1/download":
		fmt.Fprintf(w, `{"object":"signed_url","attributes":{"url":%q}}`, p.ts.URL+"/dl/backup?token=t")
	case r.Method == http.MethodDelete && path == "/backups/b-1":
		p.deleted = append(p.deleted, "b-1")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/files/list":
		fmt.Fprint(w, `{"data":[{"attributes":{"name":"server.jar"}},{"attributes":{"name":"world"}}]}`)
	case r.Method == http.MethodPost && path == "/files/compress":
		var body struct{ Files []string }
		json.NewDecoder(r.Body).Decode(&body)
		p.compress = body.Files
		fmt.Fprint(w, `{"attributes":{"name":"archive-1.tar.gz"}}`)
	case r.Method == http.MethodGet && path == "/files/download":
		if r.URL.Query().Get("file") != "/archive-1.tar.gz" {
			p.t.Errorf("download of %q", r.URL.Query().Get("file"))
		}
		fmt.Fprintf(w, `{"attributes":{"url":%q}}`, p.ts.URL+"/dl/file?token=t")
	case r.Method == http.MethodPost && path == "/files/delete":
		var body struct{ Files []string }
		json.NewDecoder(r.Body).Decode(&body)
		p.removed = append(p.removed, body.Files...)
		w.WriteHeader(http.StatusNoContent)
	default:
		p.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Uid: 988, Gid: 988}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func pteroTestServer(t *testing.T, p *fakePanel) (*Server, *[]byte) {
	s := eggTestServer(t)
	s.PteroHTTP = p.ts.Client()
	var got []byte
	var mu sync.Mutex
	s.ImportSink = func(_ context.Context, _ *models.Server, r io.Reader) error {
		b, err := io.ReadAll(r)
		mu.Lock()
		got = b
		mu.Unlock()
		return err
	}
	oldPoll := pteroPollEvery
	pteroPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { pteroPollEvery = oldPoll })
	tpl := &models.Template{
		Slug: "test-egg", Name: "Test Egg", Startup: "run", DataPath: "/home/container",
		Install: &models.InstallScript{Image: "alpine:3.20", Script: "echo install"},
		Images: []models.TemplateImage{
			{DisplayName: "Java 17", Ref: "ghcr.io/example/java:17", Default: true},
			{DisplayName: "Java 21", Ref: "ghcr.io/example/java:21"},
		},
		Variables: []models.TemplateVariable{
			{Name: "Version", EnvVariable: "VERSION", Type: models.VarString, Default: "latest", Editable: true},
			{Name: "Mode", EnvVariable: "MODE", Type: models.VarEnum, Options: []string{"a", "b"}, Default: "a", Editable: true},
			{Name: "Fixed", EnvVariable: "FIXED", Type: models.VarString, Default: "x"},
		},
	}
	if _, err := s.Store.UpsertTemplate(tpl); err != nil {
		t.Fatal(err)
	}
	return s, &got
}

func TestInspectPterodactyl(t *testing.T) {
	p := newFakePanel(t)
	s, _ := pteroTestServer(t, p)
	rr := httptest.NewRecorder()
	body := fmt.Sprintf(`{"url":%q,"apiKey":"ptlc_good"}`, p.ts.URL+"/server/1a2b3c4d/files")
	s.handleInspectPterodactyl(rr, asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), admin))
	if rr.Code != http.StatusOK {
		t.Fatalf("inspect = %d %s", rr.Code, rr.Body)
	}
	var res pteroInspectResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	d := res.Draft
	if d.Template != "test-egg" || d.Image != "ghcr.io/example/java:21" || d.Memory != "3072Mi" || d.CPU != "1500m" {
		t.Errorf("draft = %+v", d)
	}
	// No disk limit on the source: 10Gi, raised to fit 3 GiB of data with room.
	if d.Storage != "10Gi" {
		t.Errorf("storage = %q", d.Storage)
	}
	// The default allocation comes first, as TCP+UDP like a panel allocation.
	if len(d.Ports) != 2 || d.Ports[0].Port != "25565" || d.Ports[0].Protocol != "TCP/UDP" {
		t.Errorf("ports = %+v", d.Ports)
	}
	// Editable values carry over; an invalid enum value, a fixed variable and an
	// unknown one are reported instead.
	if d.Env["VERSION"] != "1.21.1" || d.Env["MODE"] != "" || d.Env["FIXED"] != "" {
		t.Errorf("env = %+v", d.Env)
	}
	w := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"MODE", "FIXED", "EXTRA"} {
		if !strings.Contains(w, want) {
			t.Errorf("no warning about %s in %q", want, w)
		}
	}

	// A wrong key is the panel's 401, reported as such.
	rr = httptest.NewRecorder()
	body = fmt.Sprintf(`{"url":%q,"apiKey":"ptlc_bad"}`, p.ts.URL+"/server/1a2b3c4d")
	s.handleInspectPterodactyl(rr, asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), admin))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "client API key") {
		t.Errorf("bad key = %d %s", rr.Code, rr.Body)
	}
	// An application key is refused before any request.
	rr = httptest.NewRecorder()
	body = fmt.Sprintf(`{"url":%q,"apiKey":"ptla_x"}`, p.ts.URL+"/server/1a2b3c4d")
	s.handleInspectPterodactyl(rr, asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), admin))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("application key = %d %s", rr.Code, rr.Body)
	}
}

func TestVolumeSizeFor(t *testing.T) {
	cases := []struct {
		limitMB, used int64
		want          string
	}{
		{0, 0, "10Gi"},
		{20000, 0, "20Gi"},
		{2048, 5 << 30, "9Gi"}, // the data does not fit the old limit: 5 GiB * 1.5 + 1
		{0, 30 << 30, "46Gi"},
	}
	for _, c := range cases {
		if got := volumeSizeFor(c.limitMB, c.used); got != c.want {
			t.Errorf("volumeSizeFor(%d, %d) = %s, want %s", c.limitMB, c.used, got, c.want)
		}
	}
}

func waitImport(t *testing.T, s *Server, id uint) *models.ImportState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv, err := s.Store.GetServer(id)
		if err != nil {
			t.Fatal(err)
		}
		if st := srv.Import; st != nil && (st.Phase == models.ImportDone || st.Phase == models.ImportFailed) {
			if _, busy := importJobs.Load(id); !busy {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("import did not finish")
	return nil
}

func createImported(t *testing.T, s *Server, p *fakePanel, start bool) *models.Server {
	t.Helper()
	body := fmt.Sprintf(`{"name":"Survival","template":"test-egg","start":%v,
	  "pterodactyl":{"url":%q,"apiKey":"ptlc_good"}}`, start, p.ts.URL+"/server/1a2b3c4d")
	rr := httptest.NewRecorder()
	s.handleCreateServer(rr, asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), admin))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rr.Code, rr.Body)
	}
	var srv models.Server
	json.Unmarshal(rr.Body.Bytes(), &srv)
	if srv.Import == nil || srv.Import.Phase != models.ImportPreparing {
		t.Errorf("created server's import = %+v", srv.Import)
	}
	// The server is created stopped, whatever Start says: the install must
	// not run before the data is in.
	if srv.DesiredState != models.StateStopped {
		t.Errorf("created %s, want Stopped", srv.DesiredState)
	}
	return &srv
}

func TestCreateWithPterodactylImportUsesABackup(t *testing.T) {
	p := newFakePanel(t)
	s, got := pteroTestServer(t, p)
	srv := createImported(t, s, p, true)
	st := waitImport(t, s, srv.ID)
	if st.Phase != models.ImportDone {
		t.Fatalf("import = %+v", st)
	}
	if !bytes.Equal(*got, p.archive) || st.Bytes != int64(len(p.archive)) {
		t.Errorf("sink got %d bytes (state says %d), want %d", len(*got), st.Bytes, len(p.archive))
	}
	if len(p.deleted) != 1 {
		t.Errorf("the panel backup was not deleted: %v", p.deleted)
	}
	after, _ := s.Store.GetServer(srv.ID)
	if after.DesiredState != models.StateRunning {
		t.Errorf("start after import: state = %s", after.DesiredState)
	}
}

func TestPterodactylImportFallsBackToCompress(t *testing.T) {
	p := newFakePanel(t)
	p.backupsRefused = true
	s, got := pteroTestServer(t, p)
	srv := createImported(t, s, p, false)
	st := waitImport(t, s, srv.ID)
	if st.Phase != models.ImportDone || len(*got) == 0 {
		t.Fatalf("import = %+v", st)
	}
	if strings.Join(p.compress, ",") != "server.jar,world" || strings.Join(p.removed, ",") != "archive-1.tar.gz" {
		t.Errorf("compressed %v, removed %v", p.compress, p.removed)
	}
	if after, _ := s.Store.GetServer(srv.ID); after.DesiredState != models.StateStopped {
		t.Errorf("state = %s, want Stopped", after.DesiredState)
	}
}

func TestPterodactylImportFailureAndRetry(t *testing.T) {
	p := newFakePanel(t)
	p.downloadFails = true
	s, _ := pteroTestServer(t, p)
	srv := createImported(t, s, p, true)
	st := waitImport(t, s, srv.ID)
	if st.Phase != models.ImportFailed || st.Message == "" {
		t.Fatalf("import = %+v", st)
	}
	// The backup made for the import is cleaned up on failure too, and a failed
	// import does not start the server.
	if len(p.deleted) != 1 {
		t.Errorf("backup not deleted: %v", p.deleted)
	}
	cur, _ := s.Store.GetServer(srv.ID)
	if cur.DesiredState != models.StateStopped {
		t.Errorf("state = %s", cur.DesiredState)
	}

	// Retry onto the existing (stopped) server.
	p.downloadFails = false
	p.polls = 0
	s.Store.UpdateServerStatus(srv.ID, models.Status{Phase: models.PhaseStopped})
	body := fmt.Sprintf(`{"url":%q,"apiKey":"ptlc_good"}`, p.ts.URL+"/server/1a2b3c4d")
	req := asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), admin)
	req.SetPathValue("id", fmt.Sprint(srv.ID))
	rr := httptest.NewRecorder()
	s.handleImportPterodactyl(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rr.Code, rr.Body)
	}
	if st := waitImport(t, s, srv.ID); st.Phase != models.ImportDone {
		t.Errorf("retry = %+v", st)
	}
}

func TestImportBlocksStart(t *testing.T) {
	s := eggTestServer(t)
	srv := &models.Server{Slug: "imp", Namespace: "qz-imp", OwnerID: admin.ID, DesiredState: models.StateStopped}
	if err := s.Store.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Store.SetServerImport(srv.ID, &models.ImportState{Phase: models.ImportDownloading, StartedAt: now, UpdatedAt: now})
	req := asUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"action":"start"}`)), admin)
	req.SetPathValue("id", fmt.Sprint(srv.ID))
	rr := httptest.NewRecorder()
	s.handlePower(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("start during import = %d %s", rr.Code, rr.Body)
	}

	// A running import whose heartbeat stopped is dead: reported as failed, and
	// it no longer blocks.
	old := now.Add(-time.Hour)
	s.Store.SetServerImport(srv.ID, &models.ImportState{Phase: models.ImportDownloading, StartedAt: old, UpdatedAt: old})
	cur, _ := s.Store.GetServer(srv.ID)
	if cur.Import.Running(time.Now()) || cur.Import.Effective(time.Now()).Phase != models.ImportFailed {
		t.Errorf("stale import = %+v", cur.Import.Effective(time.Now()))
	}
	rr = httptest.NewRecorder()
	s.handlePower(rr, req.Clone(req.Context()))
	if rr.Code != http.StatusOK {
		t.Errorf("start after a dead import = %d %s", rr.Code, rr.Body)
	}
}

// TestImportScript runs the in-pod unpack script with a real shell and tar.
func TestImportScript(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("mine"), 0o644)
	archive := tarGz(t, map[string]string{"server.jar": "jar", "world/level.dat": "lvl"})
	cmd := exec.Command("sh", "-c", guarded(importScript), root, "3")
	cmd.Stdin = bytes.NewReader(archive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	for name, want := range map[string]string{"server.jar": "jar", "world/level.dat": "lvl", "keep.txt": "mine", ".quetzal-installed": "3"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}

	// An entry climbing out of the root is not written outside it (tar strips or
	// refuses it, and the import then fails rather than half-succeeding).
	root3 := filepath.Join(t.TempDir(), "data")
	os.Mkdir(root3, 0o755)
	cmd = exec.Command("sh", "-c", guarded(importScript), root3, "1")
	cmd.Stdin = bytes.NewReader(tarGz(t, map[string]string{"../escape": "x"}))
	_ = cmd.Run()
	if _, err := os.Stat(filepath.Join(filepath.Dir(root3), "escape")); err == nil {
		t.Error("an archive entry escaped the data root")
	}

	// A corrupt archive fails the script and leaves no marker behind.
	root2 := t.TempDir()
	cmd = exec.Command("sh", "-c", guarded(importScript), root2, "1")
	cmd.Stdin = strings.NewReader("not a tarball")
	if err := cmd.Run(); err == nil {
		t.Error("a corrupt archive was accepted")
	}
	if _, err := os.Stat(filepath.Join(root2, ".quetzal-installed")); err == nil {
		t.Error("a failed import marked the server installed")
	}
}
