package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
	"github.com/lolozini/quetzal/templates"
)

// uploadHarness is a panel whose file operations run here, on a directory that
// stands for the server's data volume, through the same scripts the pod runs.
type uploadHarness struct {
	ts   *httptest.Server
	s    *Server
	st   *store.Store
	srv  *models.Server
	data string // the local directory standing for /data
	url  string // the server's uploads URL
}

func newUploadHarness(t *testing.T) (*uploadHarness, *http.Client) {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "up.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tmpl, err := st.GetTemplateBySlug("minecraft-paper") // dataPath /data
	if err != nil {
		t.Fatal(err)
	}
	alice := uploadUser(t, st, "alice")
	srv := &models.Server{
		TemplateID: tmpl.ID, Slug: "s1", Namespace: "quetzal-srv-s1", OwnerID: alice.ID,
		Image: "itzg/minecraft-server:latest", DesiredState: models.StateStopped,
		Storage: models.Storage{Type: models.StoragePVC, Size: "5Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	dataPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "data-manager-abc", Namespace: srv.Namespace, Labels: map[string]string{reconciler.DataLabel: srv.Slug}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: reconciler.WorkloadName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	s := New(st, fake.NewSimpleClientset(dataPod), &rest.Config{})
	h := &uploadHarness{s: s, st: st, srv: srv, data: filepath.Join(t.TempDir(), "data")}
	if err := os.MkdirAll(filepath.Join(h.data, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.execHook = h.run
	h.ts = httptest.NewServer(s.Handler())
	t.Cleanup(h.ts.Close)
	h.url = h.ts.URL + "/api/servers/" + strconv.FormatUint(uint64(srv.ID), 10) + "/uploads"
	return h, uploadLogin(t, h.ts.URL, "alice")
}

// run executes what would run in the pod, with /data standing for h.data.
func (h *uploadHarness) run(ctx context.Context, _ kubernetes.Interface, _ *rest.Config, _, _ string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	args := make([]string, len(cmd))
	for i, a := range cmd {
		if i >= 3 && (a == "/data" || strings.HasPrefix(a, "/data/")) {
			a = h.data + strings.TrimPrefix(a, "/data")
		}
		args[i] = a
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, &stderr
	if c.Stdin == nil {
		c.Stdin = strings.NewReader("")
	}
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &console.ExitError{Err: utilexec.CodeExitError{Err: err, Code: ee.ExitCode()}, Stderr: stderr.String()}
	}
	return err
}

func uploadUser(t *testing.T, st *store.Store, name string) *models.User {
	t.Helper()
	hash, err := auth.HashPassword(name + "pw1234")
	if err != nil {
		t.Fatal(err)
	}
	u := &models.User{Username: name, PasswordHash: hash}
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	return u
}

func uploadLogin(t *testing.T, base, name string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	b, _ := json.Marshal(map[string]string{"username": name, "password": name + "pw1234"})
	r, err := c.Post(base+"/api/login", "application/json", bytes.NewReader(b))
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("login %s: %v %v", name, err, r)
	}
	return c
}

func send(t *testing.T, c *http.Client, method, url string, body []byte, contentType string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func startUpload(t *testing.T, c *http.Client, h *uploadHarness, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	code, out := send(t, c, http.MethodPost, h.url, b, "application/json")
	id, _ := out["id"].(string)
	return code, id
}

func piece(t *testing.T, c *http.Client, h *uploadHarness, id string, offset int, data []byte) (int, int64) {
	t.Helper()
	code, out := send(t, c, http.MethodPut, h.url+"/"+id+"?offset="+strconv.Itoa(offset), data, "application/octet-stream")
	got, _ := out["received"].(float64)
	return code, int64(got)
}

// A file sent in pieces lands whole, in one step, and leaves nothing behind.
func TestUploadInPieces(t *testing.T) {
	h, c := newUploadHarness(t)
	content := bytes.Repeat([]byte("0123456789"), 10)
	if err := os.WriteFile(filepath.Join(h.data, "plugins", "big.jar"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, id := startUpload(t, c, h, map[string]any{"path": "plugins/big.jar", "size": len(content)})
	if code != http.StatusCreated || len(id) != 32 {
		t.Fatalf("start = %d %q", code, id)
	}

	if code, got := piece(t, c, h, id, 0, content[:40]); code != http.StatusOK || got != 40 {
		t.Fatalf("first piece = %d, received %d", code, got)
	}
	// Sent again, as after a lost answer: refused, and told where to go on.
	if code, got := piece(t, c, h, id, 0, content[:40]); code != http.StatusConflict || got != 40 {
		t.Fatalf("repeated piece = %d, received %d; want 409 at 40", code, got)
	}
	// The destination is untouched until the end.
	if b, _ := os.ReadFile(filepath.Join(h.data, "plugins", "big.jar")); string(b) != "old" {
		t.Fatalf("the file changed before the upload finished: %q", b)
	}
	// Finishing early is refused, and the upload stays.
	if code, _ := send(t, c, http.MethodPost, h.url+"/"+id+"/complete", nil, ""); code != http.StatusConflict {
		t.Fatalf("early finish = %d, want 409", code)
	}
	if code, out := send(t, c, http.MethodGet, h.url+"/"+id, nil, ""); code != http.StatusOK || out["received"].(float64) != 40 {
		t.Fatalf("status = %d %v", code, out)
	}
	if code, got := piece(t, c, h, id, 40, content[40:]); code != http.StatusOK || got != 100 {
		t.Fatalf("last piece = %d, received %d", code, got)
	}
	if code, _ := send(t, c, http.MethodPost, h.url+"/"+id+"/complete", nil, ""); code != http.StatusNoContent {
		t.Fatalf("finish = %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(h.data, "plugins", "big.jar")); !bytes.Equal(b, content) {
		t.Fatalf("file = %q", b)
	}
	left, _ := filepath.Glob(filepath.Join(h.data, "plugins", "*quetzal*"))
	if len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if code, _ := send(t, c, http.MethodGet, h.url+"/"+id, nil, ""); code != http.StatusNotFound {
		t.Errorf("finished upload still there: %d", code)
	}
}

// A piece cut short adds the bytes that arrived, and the next one starts from
// there: nothing to undo, nothing sent twice.
func TestUploadAppendScriptResumes(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "f.quetzal-part-x")
	run := func(offset, in string) (string, int) {
		c := exec.Command("sh", "-c", guarded(uploadAppendScript), root, tmp, offset)
		c.Stdin = strings.NewReader(in)
		out, err := c.Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return strings.TrimSpace(string(out)), ee.ExitCode()
		}
		return strings.TrimSpace(string(out)), 0
	}
	if out, code := run("0", "hello"); out != "5" || code != 0 {
		t.Fatalf("first = %q %d", out, code)
	}
	if out, code := run("3", "lo world"); out != "5" || code != fileOpConflict {
		t.Fatalf("wrong offset = %q %d, want the true size and %d", out, code, fileOpConflict)
	}
	if out, code := run("5", " world"); out != "11" || code != 0 {
		t.Fatalf("resume = %q %d", out, code)
	}
	if b, _ := os.ReadFile(tmp); string(b) != "hello world" {
		t.Fatalf("content = %q", b)
	}
}

func TestUploadArchiveInPieces(t *testing.T) {
	h, c := newUploadHarness(t)
	archive := makeTarGz(t, "level.dat", "a world")
	code, id := startUpload(t, c, h, map[string]any{"path": "world", "size": len(archive), "kind": "archive", "format": "tar"})
	if code != http.StatusCreated {
		t.Fatalf("start = %d", code)
	}
	half := len(archive) / 2
	if code, _ := piece(t, c, h, id, 0, archive[:half]); code != http.StatusOK {
		t.Fatalf("piece 1 = %d", code)
	}
	if code, _ := piece(t, c, h, id, half, archive[half:]); code != http.StatusOK {
		t.Fatalf("piece 2 = %d", code)
	}
	if code, _ := send(t, c, http.MethodPost, h.url+"/"+id+"/complete", nil, ""); code != http.StatusNoContent {
		t.Fatalf("finish = %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(h.data, "world", "level.dat")); string(b) != "a world" {
		t.Fatalf("extracted = %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(h.data, "world", ".quetzal-upload-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestUploadRules(t *testing.T) {
	h, c := newUploadHarness(t)
	bob := uploadUser(t, h.st, "bob")
	bc := uploadLogin(t, h.ts.URL, "bob")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(h.data, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.data, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{
		"climbs out":        {"path": "../x", "size": 1},
		"through a symlink": {"path": "escape/x", "size": 1},
		"no file name":      {"path": "", "size": 1},
		"names a directory": {"path": "adir", "size": 1},
		"empty":             {"path": "x", "size": 0},
		"too big":           {"path": "x", "size": maxUploadSize + 1},
		"unknown kind":      {"path": "x", "size": 1, "kind": "zipbomb"},
	} {
		if code, _ := startUpload(t, c, h, body); code != http.StatusBadRequest {
			t.Errorf("%s: start = %d, want 400", name, code)
		}
	}
	if code, _ := startUpload(t, c, h, map[string]any{"path": "nodir/x", "size": 1}); code != http.StatusNotFound {
		t.Errorf("missing directory: start = %d, want 404", code)
	}
	if code, _ := startUpload(t, bc, h, map[string]any{"path": "x", "size": 1}); code != http.StatusNotFound {
		t.Errorf("stranger: start = %d, want 404", code)
	}

	// An upload is its starter's: someone else with file access cannot add to
	// it, finish it or cancel it.
	_, id := startUpload(t, c, h, map[string]any{"path": "x", "size": 4})
	if err := h.st.GrantAccess(h.srv.ID, bob.ID, []string{models.PermView, models.PermFiles}); err != nil {
		t.Fatal(err)
	}
	if code, _ := piece(t, bc, h, id, 0, []byte("abcd")); code != http.StatusNotFound {
		t.Errorf("someone else's piece = %d, want 404", code)
	}
	if code, _ := send(t, bc, http.MethodDelete, h.url+"/"+id, nil, ""); code != http.StatusNotFound {
		t.Errorf("someone else's cancel = %d, want 404", code)
	}
	if code, _ := piece(t, c, h, id, 2, []byte("abcd")); code != http.StatusBadRequest {
		t.Errorf("piece past the size = %d, want 400", code)
	}

	// Cancelling removes what arrived.
	piece(t, c, h, id, 0, []byte("ab"))
	if code, _ := send(t, c, http.MethodDelete, h.url+"/"+id, nil, ""); code != http.StatusNoContent {
		t.Fatalf("cancel = %d", code)
	}
	if left, _ := filepath.Glob(filepath.Join(h.data, "x.quetzal-part-*")); len(left) != 0 {
		t.Errorf("cancel left %v", left)
	}

	// One request at a time.
	_, id = startUpload(t, c, h, map[string]any{"path": "y", "size": 4})
	if ok, _ := h.st.LockUpload(id, time.Now().Add(time.Minute)); !ok {
		t.Fatal("could not lock a fresh upload")
	}
	if code, _ := piece(t, c, h, id, 0, []byte("abcd")); code != http.StatusConflict {
		t.Errorf("piece while another request holds the upload = %d, want 409", code)
	}
}

// Uploads nobody finished are collected, temporary file included; those of a
// deleted server are simply forgotten.
func TestCollectUploads(t *testing.T) {
	h, c := newUploadHarness(t)
	_, id := startUpload(t, c, h, map[string]any{"path": "plugins/a.jar", "size": 10})
	piece(t, c, h, id, 0, []byte("abc"))
	tmp := filepath.Join(h.data, "plugins", "a.jar.quetzal-part-"+id)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("no temporary file: %v", err)
	}
	gone := &models.FileUpload{ID: "0123456789abcdef0123456789abcdef", ServerID: 999, UserID: 1, Path: "x", Kind: models.UploadFile, Size: 1, ExpiresAt: time.Now().Add(-time.Hour)}
	if err := h.st.CreateUpload(gone); err != nil {
		t.Fatal(err)
	}
	h.s.CollectUploads(context.Background())
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("a live upload was collected: %v", err)
	}
	if ups, _ := h.st.ExpiredUploads(); len(ups) != 0 {
		t.Errorf("the deleted server's upload is still there")
	}

	if err := h.st.TouchUpload(id, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.s.CollectUploads(context.Background())
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the expired upload's file is still there: %v", err)
	}
	if ups, _ := h.st.ExpiredUploads(); len(ups) != 0 {
		t.Errorf("the expired upload is still recorded")
	}
}
