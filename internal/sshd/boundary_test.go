package sshd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func TestRootDescriptorSurvivesReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := t.TempDir()
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "host_key"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "host_key"), []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	h := rootedHandlers(root)
	if c, ok := h.FileGet.(io.Closer); ok {
		t.Cleanup(func() { c.Close() })
	}
	if err := os.Rename(root, root+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	f, err := h.FileGet.Fileread(sftp.NewRequest("Get", "/host_key"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.(io.Closer).Close()
	b := make([]byte, 6)
	n, _ := f.ReadAt(b, 0)
	if string(b[:n]) != "inside" {
		t.Fatalf("root replaced: read %q", b[:n])
	}
}

func TestSFTPRelativeSymlinkWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	h := rootedHandlers(root)
	if c, ok := h.FileGet.(io.Closer); ok {
		t.Cleanup(func() { c.Close() })
	}
	f, err := h.FileGet.Fileread(sftp.NewRequest("Get", "/link"))
	if err != nil {
		t.Fatalf("legitimate relative link refused: %v", err)
	}
	defer f.(io.Closer).Close()
	b := make([]byte, 6)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	if string(b) != "inside" {
		t.Fatalf("read %q", b)
	}
}

func TestSFTPExistingAbsoluteSymlinkWithinRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "folder", "target"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "folder"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	h := rootedHandlers(root)
	if c, ok := h.FileGet.(io.Closer); ok {
		t.Cleanup(func() { c.Close() })
	}
	f, err := h.FileGet.Fileread(sftp.NewRequest("Get", "/link/target"))
	if err != nil {
		t.Fatalf("legitimate existing absolute parent link refused: %v", err)
	}
	defer f.(io.Closer).Close()
	b := make([]byte, 6)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	if string(b) != "inside" {
		t.Fatalf("read %q", b)
	}
}
