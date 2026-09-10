package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"errors"
	"github.com/lolozini/quetzal/internal/console"
	utilexec "k8s.io/client-go/util/exec"
	"net/http"
	"net/http/httptest"
)

func makeTarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func makeZip(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

// runExtract runs the extract script (the same one execed in the pod) against a
// real temp dir, feeding the archive on stdin.
func runExtract(t *testing.T, dir, format string, archive []byte) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", extractScript, "_", dir, format)
	cmd.Stdin = bytes.NewReader(archive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("extract output: %s", out)
	}
	return err
}

func TestExtractScriptTarGz(t *testing.T) {
	dir := t.TempDir()
	if err := runExtract(t, filepath.Join(dir, "world"), "tar", makeTarGz(t, "level.dat", "hello")); err != nil {
		t.Fatalf("extract tar.gz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "world", "level.dat"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("extracted file = %q, %v", got, err)
	}
}

func TestExtractScriptZip(t *testing.T) {
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("unzip not available on this host")
	}
	dir := t.TempDir()
	if err := runExtract(t, filepath.Join(dir, "mods"), "zip", makeZip(t, "mod.jar", "JAR")); err != nil {
		t.Fatalf("extract zip: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "mods", "mod.jar"))
	if err != nil || string(got) != "JAR" {
		t.Fatalf("extracted file = %q, %v", got, err)
	}
}

func TestExtractScriptLeavesNoTempFile(t *testing.T) {
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("unzip not available on this host")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "d")
	if err := runExtract(t, target, "zip", makeZip(t, "a.txt", "x")); err != nil {
		t.Fatalf("extract: %v", err)
	}
	entries, _ := os.ReadDir(target)
	for _, e := range entries {
		if len(e.Name()) > len(".quetzal-upload") && e.Name()[:len(".quetzal-upload")] == ".quetzal-upload" {
			t.Errorf("temp upload file left behind: %s", e.Name())
		}
	}
}

// TestWriteScriptIsAtomicAndVerified runs the real write script under /bin/sh, so
// the guarantees it encodes are checked rather than assumed: a complete payload
// lands, and a short or empty stream fails *without* destroying what was there.
func TestWriteScriptIsAtomicAndVerified(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(stdin, expected string) error {
		cmd := exec.Command("sh", "-c", writeScript, "_", target, expected)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, stderr.String())
		}
		return nil
	}
	read := func() string {
		b, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		return string(b)
	}
	leftovers := func() int {
		es, _ := os.ReadDir(dir)
		n := 0
		for _, e := range es {
			if strings.Contains(e.Name(), "quetzal-part") {
				n++
			}
		}
		return n
	}

	// A complete write replaces the file.
	if err := run("new content", "11"); err != nil {
		t.Fatalf("full write failed: %v", err)
	}
	if got := read(); got != "new content" {
		t.Errorf("target = %q, want the new content", got)
	}

	// An empty stream (the race this guards against) fails and leaves the file.
	if err := run("", "11"); err == nil {
		t.Error("empty stream should fail the write")
	}
	if got := read(); got != "new content" {
		t.Errorf("target clobbered by a lost write: %q", got)
	}

	// So does a truncated stream.
	if err := run("new", "11"); err == nil {
		t.Error("short stream should fail the write")
	}
	if got := read(); got != "new content" {
		t.Errorf("target clobbered by a short write: %q", got)
	}

	// Without an expected size the write still goes through atomically.
	if err := run("unsized", ""); err != nil {
		t.Fatalf("unsized write failed: %v", err)
	}
	if got := read(); got != "unsized" {
		t.Errorf("target = %q, want the unsized content", got)
	}

	// No temp files are left behind on any path.
	if n := leftovers(); n != 0 {
		t.Errorf("%d temp file(s) left behind", n)
	}
}

// The guard has two reasons to stop, and they belong to different people: a
// path leaving the data directory is the caller's, an unreachable data
// directory is ours. It signals which by exit code, so the API can answer 400
// instead of calling a traversal attempt a bad gateway.
func TestScriptExitCodesTellWhoseFaultItIs(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	run := func(mode, r, p string) int {
		cmd := exec.Command("sh", "-c", guardScript+`qz_guard "$1" "$0" "$2"`, r, mode, p)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			t.Fatalf("run: %v", err)
		}
		return 0
	}
	if got := run("deref", root, filepath.Join(root, "ok")); got != 0 {
		t.Errorf("a path inside the root exited %d, want 0", got)
	}
	if got := run("deref", root, filepath.Join(root, "link")); got != fileOpBadRequest {
		t.Errorf("following a symlink exited %d, want %d", got, fileOpBadRequest)
	}
	if got := run("deref", root, "/etc/passwd"); got != fileOpBadRequest {
		t.Errorf("a path outside the root exited %d, want %d", got, fileOpBadRequest)
	}
	if got := run("deref", filepath.Join(root, "gone"), "/x"); got != fileOpNoDataRoot {
		t.Errorf("a missing data root exited %d, want %d", got, fileOpNoDataRoot)
	}
}

// A path the caller simply got wrong is not an outage either: asking for a file
// that is not there used to answer 502, so a typo read as a broken data manager.
func TestMissingPathIsANotFound(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/nowhere-at-all", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := func(p string) int {
		cmd := exec.Command("sh", "-c", guardScript+`qz_exists "$1"`, "_", p)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			t.Fatalf("run: %v", err)
		}
		return 0
	}
	if got := code(filepath.Join(root, "real")); got != 0 {
		t.Errorf("an existing file exited %d, want 0", got)
	}
	if got := code(filepath.Join(root, "gone")); got != fileOpNotFound {
		t.Errorf("a missing path exited %d, want %d", got, fileOpNotFound)
	}
	// A broken link is still something the panel must be able to see and remove.
	if got := code(filepath.Join(root, "dangling")); got != 0 {
		t.Errorf("a dangling symlink exited %d, want 0 (it must stay listable and deletable)", got)
	}

	w := httptest.NewRecorder()
	writeFileOpError(w, "read failed", &console.ExitError{
		Err:    utilexec.CodeExitError{Err: errors.New("command terminated"), Code: fileOpNotFound},
		Stderr: "no such file or directory",
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("a missing path answered %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no such file or directory") {
		t.Errorf("the reason was lost: %s", w.Body.String())
	}
}

func TestRefusedPathIsNotABadGateway(t *testing.T) {
	refused := &console.ExitError{
		Err:    utilexec.CodeExitError{Err: errors.New("command terminated"), Code: fileOpBadRequest},
		Stderr: "path escapes the data directory",
	}
	w := httptest.NewRecorder()
	writeFileOpError(w, "list failed", refused)
	if w.Code != http.StatusBadRequest {
		t.Errorf("refused path answered %d, want 400: a traversal attempt is not an outage", w.Code)
	}
	if !strings.Contains(w.Body.String(), "path escapes the data directory") {
		t.Errorf("the reason was lost: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "command terminated") {
		t.Errorf("the exec plumbing leaked into the answer: %s", w.Body.String())
	}

	// Anything else is still ours to own.
	for _, err := range []error{
		&console.ExitError{Err: utilexec.CodeExitError{Err: errors.New("x"), Code: fileOpNoDataRoot}, Stderr: "the data directory is not available"},
		&console.ExitError{Err: utilexec.CodeExitError{Err: errors.New("x"), Code: 2}, Stderr: "tar: not an archive"},
		errors.New("connection reset"),
	} {
		w := httptest.NewRecorder()
		writeFileOpError(w, "extract failed", err)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%v answered %d, want 502", err, w.Code)
		}
	}
}

// The checks were inserted into scripts that already worked; run them against a
// real directory to be sure they still do the job they were written for.
func TestFileScriptsStillWorkOnPathsThatAreThere(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(script string, args ...string) (string, int) {
		cmd := exec.Command("sh", append([]string{"-c", guardScript + script, root}, args...)...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return out.String(), ee.ExitCode()
			}
			t.Fatalf("run: %v", err)
		}
		return out.String(), 0
	}
	if out, code := run(listScript, root); code != 0 || !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub") {
		t.Errorf("listing a real directory: code=%d out=%q", code, out)
	}
	if _, code := run(listScript, filepath.Join(root, "a.txt")); code != fileOpBadRequest {
		t.Errorf("listing a file exited %d, want %d (not a directory)", code, fileOpBadRequest)
	}
	if _, code := run(listScript, filepath.Join(root, "gone")); code != fileOpNotFound {
		t.Errorf("listing a missing directory exited %d, want %d", code, fileOpNotFound)
	}
	readScript := `qz_guard deref "$0" "$1"
qz_exists "$1"
[ -d "$1" ] && { echo "is a directory" >&2; exit 4; }
exec cat -- "$1"`
	if out, code := run(readScript, filepath.Join(root, "a.txt")); code != 0 || out != "hello" {
		t.Errorf("reading a real file: code=%d out=%q", code, out)
	}
	if _, code := run(readScript, filepath.Join(root, "sub")); code != fileOpBadRequest {
		t.Errorf("reading a directory exited %d, want %d", code, fileOpBadRequest)
	}
	if _, code := run(readScript, filepath.Join(root, "gone")); code != fileOpNotFound {
		t.Errorf("reading a missing file exited %d, want %d", code, fileOpNotFound)
	}
}
