package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
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
