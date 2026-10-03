package sshd

import (
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/authkeys"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// keysFor lets pubs in under any name, as a file without accounts does.
func keysFor(pubs ...ssh.PublicKey) []authkeys.Key {
	out := make([]authkeys.Key, 0, len(pubs))
	for _, p := range pubs {
		out = append(out, authkeys.Key{Key: p})
	}
	return out
}

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
		AuthorizedKeys: func() []authkeys.Key { return keysFor(authorized...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })

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

func TestSFTPRevokesLiveSession(t *testing.T) {
	root := t.TempDir()
	signer, pub := newKeyPair(t)

	// A mutable authorized set, guarded for the revoke loop's concurrent reads.
	var mu sync.Mutex
	authorized := []ssh.PublicKey{pub}

	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		Addr: "127.0.0.1:0", Root: root, HostKey: hostKey,
		RevokeCheckInterval: 25 * time.Millisecond,
		AuthorizedKeys: func() []authkeys.Key {
			mu.Lock()
			defer mu.Unlock()
			return keysFor(authorized...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })

	conn, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
		User:            "anyone",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         2 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	sc, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer sc.Close()

	// The session works while authorized.
	if _, err := sc.ReadDir("/"); err != nil {
		t.Fatalf("readdir before revoke: %v", err)
	}

	// Revoke the key; the live session must be dropped, not just blocked next time.
	mu.Lock()
	authorized = nil
	mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sc.ReadDir("/"); err != nil {
			return // session was cut, as intended
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("live session was not dropped after the key was revoked")
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

// A symlink planted in the volume (an extracted archive, or the game process)
// must not become a way out of the jail: textual confinement alone would let
// "/link.txt" resolve inside the root and still hand back a file outside it.
func TestSFTPSymlinkConfinement(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link to a file outside, and a link to the directory outside.
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(root), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKeyPair(t)
	sc, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sc.Close()

	for _, p := range []string{"/link.txt", "/escape/secret.txt"} {
		if f, err := sc.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("symlink %q leaked: %q", p, b)
		}
	}
	// Writing through a symlink would clobber a file outside the volume.
	if f, err := sc.OpenFile("/link.txt", os.O_WRONLY); err == nil {
		f.Close()
		t.Error("opened a symlink for writing")
	}
	if got, _ := os.ReadFile(outside); string(got) != "TOPSECRET" {
		t.Errorf("file outside the root was modified: %q", got)
	}
	// Ordinary files must keep working, and the link itself must stay removable
	// so a planted one can be cleaned up through SFTP.
	f, err := sc.Open("/ok.txt")
	if err != nil {
		t.Fatalf("open ok.txt: %v", err)
	}
	f.Close()
	if err := sc.Remove("/link.txt"); err != nil {
		t.Errorf("could not remove the symlink: %v", err)
	}
}

// OpenSSH's sftp sends a symlink's target first and the link second, and so
// does pkg/sftp's client. Taken the other way round, `symlink /etc qa/sftplink`
// made a link named etc at the root, pointing at qa/sftplink: the feature did
// nothing useful and left a dangling link behind. The link is where the client
// said, and what it points at stays inside the root.
func TestSFTPSymlinkGoesWhereTheClientSaid(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "qa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKeyPair(t)
	sc, _, err := startServer(t, root, []ssh.PublicKey{pub}, signer)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sc.Close()

	for _, c := range []struct{ target, link, want string }{
		{"/server.properties", "qa/props", "server.properties"},
		{"/etc", "qa/sftplink", "etc"},
		{"../server.properties", "/qa/relative", "server.properties"},
		{"/../../outside", "qa/escape", "outside"},
	} {
		if err := sc.Symlink(c.target, c.link); err != nil {
			t.Errorf("symlink %s -> %s: %v", c.link, c.target, err)
			continue
		}
		got, err := os.Readlink(filepath.Join(root, c.link))
		if err != nil {
			t.Errorf("symlink %s -> %s: no link at %s (%v)", c.link, c.target, c.link, err)
			continue
		}
		if want := filepath.Join(root, c.want); got != want {
			t.Errorf("symlink %s -> %s points at %s, want %s", c.link, c.target, got, want)
		}
	}
	if fi, err := os.Lstat(filepath.Join(root, "server.properties")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the target was touched: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc")); err == nil {
		t.Error("a link named after the target appeared at the root")
	}
}

// SFTP took any name with any key: the recette of 0.10.0 signed in as qa-admin
// with another account's key (R-10). A key goes with its accounts' names now,
// and what a session changes is told, with who changed it.
func TestAKeyGoesWithItsAccountAndChangesAreLogged(t *testing.T) {
	root := t.TempDir()
	signer, pub := newKeyPair(t)
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logged []string
	srv, err := New(Config{
		Addr: "127.0.0.1:0", Root: root, HostKey: hostKey,
		AuthorizedKeys: func() []authkeys.Key { return []authkeys.Key{{Key: pub, Users: []string{"alice"}}} },
		LogOp: func(user, op, p string) {
			mu.Lock()
			logged = append(logged, user+" "+op+" "+p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close() })
	<-srv.ready
	dial := func(user string) (*sftp.Client, error) {
		conn, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { conn.Close() })
		return sftp.NewClient(conn)
	}
	if _, err := dial("mallory"); err == nil {
		t.Fatal("alice's key signed in as mallory")
	}
	c, err := dial("Alice")
	if err != nil {
		t.Fatalf("alice's key, as Alice: %v", err)
	}
	f, err := c.Create("/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hi"))
	f.Close()
	if err := c.Rename("/notes.txt", "/kept.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("/kept.txt"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Alice write /notes.txt", "Alice rename /notes.txt -> /kept.txt", "Alice remove /kept.txt"}
	if strings.Join(logged, "\n") != strings.Join(want, "\n") {
		t.Errorf("logged:\n%s\nwant:\n%s", strings.Join(logged, "\n"), strings.Join(want, "\n"))
	}
}
