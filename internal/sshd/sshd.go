// Package sshd implements a minimal, key-only SFTP server that serves a single
// directory tree (a server's data volume), confined to that root. It is run as a
// sidecar in the game image (so files are owned by the server's user) and
// authenticates SSH public keys against an authorized_keys file that the control
// plane keeps in sync with the users who hold file access.
package sshd

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// defaultRevokeInterval is how often live sessions are re-checked against the
// authorized keys so a revoked key is cut off, not just blocked at next connect.
const defaultRevokeInterval = 30 * time.Second

// pubkeyExt carries the authenticated public key (base64 of its wire form) from
// the auth callback to the connection handler, so we can re-check it later.
const pubkeyExt = "quetzal-pubkey"

// Config configures the server.
type Config struct {
	Addr    string // listen address, e.g. ":2022"
	Root    string // directory served as "/"
	HostKey []byte // PEM-encoded host private key
	// AuthorizedKeys returns the currently-authorized public keys. It is called
	// on every authentication attempt so changes apply without a restart.
	AuthorizedKeys func() []ssh.PublicKey
	// RevokeCheckInterval is how often open sessions are re-checked against
	// AuthorizedKeys; a session whose key is no longer authorized is closed.
	// Defaults to defaultRevokeInterval.
	RevokeCheckInterval time.Duration
}

// Server is a key-only SFTP server.
type Server struct {
	cfg      Config
	sshConf  *ssh.ServerConfig
	listener net.Listener

	done  chan struct{}
	ready chan struct{} // closed once Serve has bound (or failed to bind)
	mu    sync.Mutex
	// conns maps each live connection to the wire form of the key it
	// authenticated with, so the revoke loop can drop sessions whose key is
	// no longer authorized.
	conns map[*ssh.ServerConn]string
}

// New validates the config and prepares the SSH server config.
func New(cfg Config) (*Server, error) {
	if cfg.Root == "" {
		return nil, errors.New("sshd: root is required")
	}
	if cfg.AuthorizedKeys == nil {
		return nil, errors.New("sshd: AuthorizedKeys is required")
	}
	signer, err := ssh.ParsePrivateKey(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("sshd: host key: %w", err)
	}
	sc := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			want := key.Marshal()
			for _, ak := range cfg.AuthorizedKeys() {
				if subtle.ConstantTimeCompare(want, ak.Marshal()) == 1 {
					return &ssh.Permissions{Extensions: map[string]string{
						pubkeyExt: base64.StdEncoding.EncodeToString(want),
					}}, nil
				}
			}
			return nil, fmt.Errorf("sshd: unauthorized key")
		},
	}
	sc.AddHostKey(signer)
	return &Server{
		cfg:     cfg,
		sshConf: sc,
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		conns:   make(map[*ssh.ServerConn]string),
	}, nil
}

// Serve listens and serves until the listener is closed. It accepts connections
// in a loop; per-connection errors are returned via the logger, not fatal.
func (s *Server) Serve() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		close(s.ready) // unblock Addr() even on bind failure
		return err
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	close(s.ready)
	go s.revokeLoop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err // listener closed
		}
		go s.handleConn(conn)
	}
}

// Addr returns the bound address (useful when Addr was ":0" in tests). It blocks
// until the listener is bound.
func (s *Server) Addr() net.Addr {
	<-s.ready
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Close stops accepting connections and the revoke loop.
func (s *Server) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		return ln.Close()
	}
	return nil
}

// revokeLoop periodically drops live sessions whose key is no longer authorized.
func (s *Server) revokeLoop() {
	interval := s.cfg.RevokeCheckInterval
	if interval <= 0 {
		interval = defaultRevokeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.dropRevoked()
		}
	}
}

// dropRevoked closes any open connection whose authenticated key is no longer
// returned by AuthorizedKeys (revoked key, or a subuser who lost file access).
func (s *Server) dropRevoked() {
	authorized := make(map[string]bool)
	for _, ak := range s.cfg.AuthorizedKeys() {
		authorized[string(ak.Marshal())] = true
	}
	s.mu.Lock()
	var revoked []*ssh.ServerConn
	for c, blob := range s.conns {
		if !authorized[blob] {
			revoked = append(revoked, c)
		}
	}
	s.mu.Unlock()
	for _, c := range revoked {
		_ = c.Close()
	}
}

func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(c, s.sshConf)
	if err != nil {
		return // failed handshake/auth
	}
	defer sconn.Close()

	// Track the connection by the key it authenticated with so the revoke loop
	// can cut it off if that key is later removed.
	var blob string
	if sconn.Permissions != nil {
		if raw, err := base64.StdEncoding.DecodeString(sconn.Permissions.Extensions[pubkeyExt]); err == nil {
			blob = string(raw)
		}
	}
	s.mu.Lock()
	s.conns[sconn] = blob
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sconn)
		s.mu.Unlock()
	}()

	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.serveSession(ch, chReqs)
	}
}

func (s *Server) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	// Accept only the "sftp" subsystem request.
	go func() {
		for r := range reqs {
			ok := r.Type == "subsystem" && len(r.Payload) >= 4 && string(r.Payload[4:]) == "sftp"
			if r.WantReply {
				_ = r.Reply(ok, nil)
			}
		}
	}()
	h := rootedHandlers(s.cfg.Root)
	srv := sftp.NewRequestServer(ch, h)
	defer srv.Close()
	_ = srv.Serve()
}

// ---- rooted (chroot-like) handlers ----

// root confines all client paths under a base directory: the client sees base as
// "/", and ".." can never escape it.
type root struct{ base string }

func rootedHandlers(base string) sftp.Handlers {
	r := &root{base: base}
	return sftp.Handlers{FileGet: r, FilePut: r, FileCmd: r, FileList: r}
}

// resolve maps a client path to a real path confined under base.
func (r *root) resolve(p string) string {
	return filepath.Join(r.base, filepath.Clean("/"+p))
}

func (r *root) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	return os.OpenFile(r.resolve(req.Filepath), os.O_RDONLY, 0)
}

func (r *root) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	p := r.resolve(req.Filepath)
	flags := os.O_RDWR | os.O_CREATE
	pf := req.Pflags()
	if pf.Trunc {
		flags |= os.O_TRUNC
	}
	if pf.Excl {
		flags |= os.O_EXCL
	}
	// Deliberately not honoring pf.Append: the request server writes via
	// WriteAt at explicit offsets, which is invalid on a file opened O_APPEND.
	return os.OpenFile(p, flags, 0o644)
}

func (r *root) Filecmd(req *sftp.Request) error {
	p := r.resolve(req.Filepath)
	switch req.Method {
	case "Setstat":
		return r.setstat(p, req)
	case "Rename":
		return os.Rename(p, r.resolve(req.Target))
	case "Rmdir", "Remove":
		return os.Remove(p)
	case "Mkdir":
		return os.MkdirAll(p, 0o755)
	case "Symlink":
		// Link target stays within the root.
		return os.Symlink(r.resolve(req.Target), p)
	default:
		return sftp.ErrSSHFxOpUnsupported
	}
}

func (r *root) setstat(p string, req *sftp.Request) error {
	attr := req.Attributes()
	if req.AttrFlags().Size {
		if err := os.Truncate(p, int64(attr.Size)); err != nil {
			return err
		}
	}
	if req.AttrFlags().Permissions {
		if err := os.Chmod(p, attr.FileMode()); err != nil {
			return err
		}
	}
	return nil
}

func (r *root) Filelist(req *sftp.Request) (sftp.ListerAt, error) {
	p := r.resolve(req.Filepath)
	switch req.Method {
	case "List":
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				infos = append(infos, fi)
			}
		}
		return listerat(infos), nil
	case "Stat":
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		return listerat{fi}, nil
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
}

// listerat adapts a slice of FileInfo to sftp.ListerAt.
type listerat []os.FileInfo

func (l listerat) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}
