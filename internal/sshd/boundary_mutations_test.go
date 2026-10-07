package sshd

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSFTPBoundRootMutations(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, outside} {
		for _, name := range []string{"write", "rename", "remove", "mode"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("sentinel"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	signer, pub := newKeyPair(t)
	c, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Ensure the session has opened its root before replacing its pathname.
	if _, err := c.Stat("write"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	f, err := c.Create("write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("rename", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("remove"); err != nil {
		t.Fatal(err)
	}
	if err := c.Chmod("mode", 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Truncate("mode", 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir("new"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"write", "rename", "remove", "mode"} {
		b, err := os.ReadFile(filepath.Join(outside, name))
		if err != nil || string(b) != "sentinel" {
			t.Errorf("outside %s changed: %q %v", name, b, err)
		}
	}
	fi, err := os.Stat(filepath.Join(outside, "mode"))
	if err != nil || fi.Mode().Perm() != 0644 {
		t.Errorf("outside mode changed: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Errorf("outside directory created: %v", err)
	}
}
