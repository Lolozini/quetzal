package sshd

import (
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// newKeyPair returns an ssh signer and its public key.
func newKeyPair(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer, sshPub
}

// startServer starts a server authorizing `authorized` and returns an SFTP
// client authenticated with `client`. Errors connecting are returned.
func startServer(t *testing.T, root string, authorized []ssh.PublicKey, client ssh.Signer) (*sftp.Client, *Server, error) {
	t.Helper()
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		Addr: "127.0.0.1:0", Root: root, HostKey: hostKey,
		AuthorizedKeys: func() []ssh.PublicKey { return authorized },
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	// Wait for the listener.
	deadline := time.Now().Add(2 * time.Second)
	for srv.listener == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	conn, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
		User:            "anyone",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(client)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	})
	if err != nil {
		return nil, srv, err
	}
	t.Cleanup(func() { conn.Close() })
	sc, err := sftp.NewClient(conn)
	return sc, srv, err
}

func TestSFTPAuthorizedKeyRoundTrip(t *testing.T) {
	root := t.TempDir()
	signer, pub := newKeyPair(t)
	sc, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sc.Close()

	// Write a file.
	f, err := sc.Create("/hello.txt")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	// It exists on disk under the root.
	if b, err := os.ReadFile(filepath.Join(root, "hello.txt")); err != nil || string(b) != "data" {
		t.Fatalf("on-disk = %q, %v", b, err)
	}

	// Read it back over SFTP.
	rf, err := sc.Open("/hello.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, _ := io.ReadAll(rf)
	rf.Close()
	if string(got) != "data" {
		t.Errorf("read = %q, want data", got)
	}

	// Mkdir + rename + list + remove.
	if err := sc.Mkdir("/sub"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := sc.Rename("/hello.txt", "/sub/moved.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	entries, err := sc.ReadDir("/")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var sawSub bool
	for _, e := range entries {
		if e.Name() == "sub" && e.IsDir() {
			sawSub = true
		}
	}
	if !sawSub {
		t.Error("listing did not include the sub directory")
	}
	if _, err := sc.Stat("/sub/moved.txt"); err != nil {
		t.Errorf("stat moved: %v", err)
	}
	if err := sc.Remove("/sub/moved.txt"); err != nil {
		t.Errorf("remove: %v", err)
	}
}

func TestSFTPPathConfinement(t *testing.T) {
	root := t.TempDir()
	// A secret outside the root must be unreachable via traversal.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKeyPair(t)
	sc, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sc.Close()

	for _, p := range []string{"/../secret.txt", "/../../secret.txt", "/sub/../../secret.txt"} {
		if f, err := sc.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("traversal %q leaked: %q", p, b)
		}
	}
}

func TestSFTPRejectsUnauthorizedKey(t *testing.T) {
	root := t.TempDir()
	clientSigner, _ := newKeyPair(t)
	_, otherPub := newKeyPair(t) // a different key is the only authorized one
	_, _, err := startServer(t, root, []ssh.PublicKey{otherPub}, clientSigner)
	if err == nil {
		t.Fatal("expected authentication to fail for an unauthorized key")
	}
}
