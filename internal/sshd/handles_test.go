package sshd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSFTPHandlesStayBoundAcrossLeafAndParentSwaps(t *testing.T) {
	for _, parent := range []bool{false, true} {
		name := "leaf"
		if parent {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(root, "dir")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			inside := filepath.Join(dir, "host_key")
			secret := filepath.Join(outside, "host_key")
			if err := os.WriteFile(inside, []byte("inside"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secret, []byte("SECRET"), 0600); err != nil {
				t.Fatal(err)
			}
			signer, pub := newKeyPair(t)
			client, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			reader, err := client.Open("dir/host_key")
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer, err := client.OpenFile("dir/host_key", os.O_WRONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if parent {
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(inside, inside+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(secret, inside); err != nil {
					t.Fatal(err)
				}
			}
			b, err := io.ReadAll(reader)
			if err != nil || string(b) != "inside" {
				t.Fatalf("bound read = %q %v", b, err)
			}
			if _, err := writer.WriteAt([]byte("update"), 0); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if f, err := client.Open("dir/host_key"); err == nil {
				f.Close()
				t.Error("new open followed outside replacement")
			}
			if err := client.Chmod("dir/host_key", 0777); err == nil {
				t.Error("chmod followed outside replacement")
			}
			if err := client.Truncate("dir/host_key", 0); err == nil {
				t.Error("truncate followed outside replacement")
			}
			b, err = os.ReadFile(secret)
			if err != nil || string(b) != "SECRET" {
				t.Fatalf("outside changed = %q %v", b, err)
			}
			info, err := os.Stat(secret)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("outside mode changed: %v %v", info, err)
			}
		})
	}
}

func TestSFTPChmodUnreadableFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "unreadable")
	if err := os.WriteFile(target, []byte("inside"), 0000); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKeyPair(t)
	client, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Chmod("unreadable", 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "inside" {
		t.Fatalf("chmod did not restore access: %q %v", b, err)
	}
}
