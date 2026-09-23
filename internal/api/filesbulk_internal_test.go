package api

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runScript runs a guarded file script with a real shell, the data root as $0.
func runScript(t *testing.T, root, script string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", guarded(script), root}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), errb.String(), ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return out.String(), errb.String(), 0
}

func mkfile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestListScriptReportsModTime(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "a b.txt"), "hello")
	when := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(root, "a b.txt"), when, when)
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	out, _, code := runScript(t, root, listScript, root)
	if code != 0 {
		t.Fatalf("list exited %d", code)
	}
	want := "f\t5\t" + "1740830400" + "\ta b.txt"
	if !strings.Contains(out, want) || !strings.Contains(out, "d\t0\t") {
		t.Errorf("listing = %q, want a line %q", out, want)
	}
}

func TestCopyCandidates(t *testing.T) {
	cases := map[string][]string{
		"server.properties": {"server copy.properties", "server copy 1.properties"},
		"world":             {"world copy", "world copy 1"},
		"backup.tar.gz":     {"backup copy.tar.gz", "backup copy 1.tar.gz"},
		".env":              {".env copy", ".env copy 1"},
	}
	for in, want := range cases {
		got := copyCandidates(in)
		if got[0] != want[0] || got[1] != want[1] || len(got) != 51 {
			t.Errorf("copyCandidates(%q) = %q…", in, got[:2])
		}
	}
}

func TestCopyScript(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "cfg.yml"), "a: 1")
	mkfile(t, filepath.Join(root, "world", "level.dat"), "lvl")
	os.Symlink("/etc/passwd", filepath.Join(root, "world", "link"))
	cands := func(name string) []string {
		var out []string
		for _, c := range copyCandidates(name) {
			out = append(out, filepath.Join(root, c))
		}
		return out
	}

	out, stderr, code := runScript(t, root, copyScript, append([]string{filepath.Join(root, "cfg.yml")}, cands("cfg.yml")...)...)
	if code != 0 || filepath.Base(out) != "cfg copy.yml" {
		t.Fatalf("copy: %q %q %d", out, stderr, code)
	}
	// The next copy takes the next free name.
	out, _, _ = runScript(t, root, copyScript, append([]string{filepath.Join(root, "cfg.yml")}, cands("cfg.yml")...)...)
	if filepath.Base(out) != "cfg copy 1.yml" {
		t.Errorf("second copy = %q", out)
	}
	// A folder is copied whole; a link inside it stays a link.
	out, stderr, code = runScript(t, root, copyScript, append([]string{filepath.Join(root, "world")}, cands("world")...)...)
	if code != 0 {
		t.Fatalf("folder copy: %q %d", stderr, code)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "world copy", "level.dat")); string(b) != "lvl" {
		t.Errorf("folder copy content = %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(root, "world copy", "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link inside the folder was not copied as a link: %v", err)
	}
	// A candidate outside the root is refused.
	_, _, code = runScript(t, root, copyScript, filepath.Join(root, "cfg.yml"), "/tmp/escape-"+filepath.Base(root))
	if code != fileOpBadRequest || exists("/tmp/escape-"+filepath.Base(root)) {
		t.Errorf("copy outside the root exited %d", code)
	}
}

func TestBulkDeleteAndMoveScripts(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.log", "b.log", "keep.txt", "logs/old.log"} {
		mkfile(t, filepath.Join(root, f), f)
	}
	outside := t.TempDir()
	mkfile(t, filepath.Join(outside, "victim"), "x")
	os.Symlink(outside, filepath.Join(root, "out"))

	// Move two files into a folder.
	_, stderr, code := runScript(t, root, moveScript, filepath.Join(root, "logs"), filepath.Join(root, "a.log"), filepath.Join(root, "b.log"))
	if code != 0 || !exists(filepath.Join(root, "logs", "a.log")) || exists(filepath.Join(root, "a.log")) {
		t.Fatalf("move: %q %d", stderr, code)
	}
	// Nothing is overwritten: an existing name stops the whole move.
	mkfile(t, filepath.Join(root, "a.log"), "new")
	_, stderr, code = runScript(t, root, moveScript, filepath.Join(root, "logs"), filepath.Join(root, "keep.txt"), filepath.Join(root, "a.log"))
	if code != fileOpBadRequest || !strings.Contains(stderr, "already exists") || !exists(filepath.Join(root, "keep.txt")) {
		t.Errorf("move over an existing file: %q %d", stderr, code)
	}
	// Moving into a link that leaves the root is refused.
	_, _, code = runScript(t, root, moveScript, filepath.Join(root, "out"), filepath.Join(root, "keep.txt"))
	if code != fileOpBadRequest || exists(filepath.Join(outside, "keep.txt")) {
		t.Errorf("move through a link exited %d", code)
	}

	// Delete several entries, including a link (the link goes, not its target).
	_, stderr, code = runScript(t, root, bulkDeleteScript, filepath.Join(root, "a.log"), filepath.Join(root, "logs"), filepath.Join(root, "out"))
	if code != 0 || exists(filepath.Join(root, "logs")) || exists(filepath.Join(root, "out")) || !exists(filepath.Join(outside, "victim")) {
		t.Errorf("delete: %q %d", stderr, code)
	}
	if !exists(filepath.Join(root, "keep.txt")) {
		t.Error("an entry that was not named was deleted")
	}
}

func TestCompressAndDecompressScripts(t *testing.T) {
	root := t.TempDir()
	mkfile(t, filepath.Join(root, "world", "level.dat"), "lvl")
	mkfile(t, filepath.Join(root, "-rf"), "dash")
	mkfile(t, filepath.Join(root, "server.properties"), "p=1")

	out, stderr, code := runScript(t, root, compressScript, root, "archive-1.tar.gz", "world", "-rf")
	if code != 0 || out != "archive-1.tar.gz" {
		t.Fatalf("compress: %q %q %d", out, stderr, code)
	}
	if m, _ := filepath.Glob(filepath.Join(root, ".*quetzal-part*")); len(m) > 0 {
		t.Errorf("temporary left behind: %v", m)
	}
	// A folder that is not there is a 404, not a tar failure.
	if _, _, code := runScript(t, root, compressScript, filepath.Join(root, "gone"), "a.tar.gz", "x"); code != fileOpNotFound {
		t.Errorf("compress in a missing folder exited %d", code)
	}
	// It refuses to overwrite an archive of the same name.
	if _, _, code := runScript(t, root, compressScript, root, "archive-1.tar.gz", "world"); code != fileOpBadRequest {
		t.Errorf("second compress exited %d", code)
	}

	// Unpack into another folder: the archive is moved there first.
	os.Mkdir(filepath.Join(root, "restore"), 0o755)
	os.Rename(filepath.Join(root, "archive-1.tar.gz"), filepath.Join(root, "restore", "archive-1.tar.gz"))
	if _, stderr, code := runScript(t, root, decompressScript, filepath.Join(root, "restore", "archive-1.tar.gz"), "z"); code != 0 {
		t.Fatalf("decompress: %q %d", stderr, code)
	}
	for f, want := range map[string]string{"restore/world/level.dat": "lvl", "restore/-rf": "dash"} {
		if b, _ := os.ReadFile(filepath.Join(root, f)); string(b) != want {
			t.Errorf("%s = %q", f, b)
		}
	}
	// A directory is not an archive.
	if _, _, code := runScript(t, root, decompressScript, filepath.Join(root, "world"), "z"); code != fileOpBadRequest {
		t.Errorf("decompressing a folder exited %d", code)
	}
}

func TestDecompressZip(t *testing.T) {
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("no unzip here")
	}
	root := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("mods/a.jar")
	f.Write([]byte("jar"))
	zw.Close()
	mkfile(t, filepath.Join(root, "pack.zip"), buf.String())
	if _, stderr, code := runScript(t, root, decompressScript, filepath.Join(root, "pack.zip"), "zip"); code != 0 {
		t.Fatalf("unzip: %q %d", stderr, code)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "mods", "a.jar")); string(b) != "jar" {
		t.Errorf("mods/a.jar = %q", b)
	}
}

func TestArchiveFormatAndNames(t *testing.T) {
	for name, want := range map[string]string{"a.zip": "zip", "A.TGZ": "z", "w.tar.xz": "J", "b.tar.bz2": "j", "x.tar": "-", "y.rar": "", "z.gz": ""} {
		if got, _ := archiveFormat(name); got != want {
			t.Errorf("archiveFormat(%q) = %q, want %q", name, got, want)
		}
	}
	for _, bad := range [][]string{nil, {""}, {"."}, {".."}, {"a/b"}, {"a", "a"}} {
		if checkNames(bad) == nil {
			t.Errorf("checkNames(%q) accepted", bad)
		}
	}
	if err := checkNames([]string{"a", ".hidden", "with space"}); err != nil {
		t.Errorf("checkNames: %v", err)
	}
}
