package sshd

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lolozini/quetzal/internal/crypto"
)

// startBare starts a server with no authorized keys and returns its address. No
// client here ever authenticates: the point is what happens to one that doesn't.
func startBare(t *testing.T, cfg Config) *Server {
	t.Helper()
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = "127.0.0.1:0"
	cfg.Root = t.TempDir()
	cfg.HostKey = hostKey
	cfg.AuthorizedKeys = func() []ssh.PublicKey { return nil }
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	if srv.Addr() == nil {
		t.Fatal("server did not bind")
	}
	return srv
}

// A connection that opens a socket and then says nothing must be dropped. This
// server is a sidecar in the game server's own pod, on a NodePort: without a
// deadline, holding thousands of these costs an attacker nothing and costs the
// pod its memory limit.
func TestSilentConnectionIsDropped(t *testing.T) {
	srv := startBare(t, Config{HandshakeTimeout: 300 * time.Millisecond})

	c, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Read past the server's version banner and then wait to be cut off. Without
	// the deadline this read blocks until the test's own deadline instead.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	start := time.Now()
	for {
		_, err := c.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("connection still open after %s: no handshake deadline", time.Since(start))
			}
			break // EOF / reset: the server dropped us, which is the point
		}
		if time.Since(start) > 4*time.Second {
			t.Fatal("connection still open: no handshake deadline")
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s to drop a silent connection", d)
	}
}

// Beyond the cap, a new connection is closed on accept rather than given a
// goroutine. The slot is held only until the handshake resolves, so this bounds
// what an unauthenticated flood can tie up.
func TestPendingHandshakeCap(t *testing.T) {
	srv := startBare(t, Config{
		HandshakeTimeout:     5 * time.Second,
		MaxPendingHandshakes: 1,
	})

	// Occupy the single slot with a connection that never speaks.
	hog, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer hog.Close()
	// Wait until the server has actually accepted it and started the handshake
	// (it writes its version banner first).
	_ = hog.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := hog.Read(make([]byte, 64)); err != nil {
		t.Fatalf("no banner from the server: %v", err)
	}

	second, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := second.Read(make([]byte, 64))
	if err == nil {
		t.Fatalf("second connection got %d bytes; it should have been closed unserved", n)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("second connection was left hanging instead of closed")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		// A reset is equally acceptable; only a served or hung connection is not.
		t.Logf("closed with %v", err)
	}
}

// An authenticated session must not inherit the handshake deadline: SFTP sits
// idle between operations, and a deadline that is right for an anonymous
// connection would disconnect a working client mid-transfer.
func TestAuthenticatedSessionOutlivesHandshakeTimeout(t *testing.T) {
	signer, pub := newKeyPair(t)
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{
		Addr: "127.0.0.1:0", Root: t.TempDir(), HostKey: hostKey,
		HandshakeTimeout: 250 * time.Millisecond,
		AuthorizedKeys:   func() []ssh.PublicKey { return []ssh.PublicKey{pub} },
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	cl, err := ssh.Dial("tcp", srv.Addr().String(), &ssh.ClientConfig{
		User:            "quetzal",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// Idle well past the handshake deadline, then use the connection.
	time.Sleep(600 * time.Millisecond)
	if _, _, err := cl.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("session died after the handshake deadline elapsed: %v", err)
	}
}
