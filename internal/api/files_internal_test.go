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
