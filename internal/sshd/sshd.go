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
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// defaultRevokeInterval is how often live sessions are re-checked against the
// authorized keys so a revoked key is cut off, not just blocked at next connect.
const defaultRevokeInterval = 30 * time.Second

// defaultHandshakeTimeout bounds a connection that has not authenticated yet.
// Without one, a client that opens a socket and then says nothing holds a
// goroutine and a file descriptor for as long as it likes -- and this server is
// a sidecar in the game server's own pod, sharing its memory limit, published on
// a NodePort. Enough silent connections and the kubelet OOM-kills the pod: the
// game goes down, from the internet, with no credentials at all. A real SSH
// handshake finishes in well under a second.
const defaultHandshakeTimeout = 15 * time.Second

// defaultMaxPending bounds how many connections may be mid-handshake at once, so
// the memory that can be tied up before anyone proves who they are is bounded by
// a constant rather than by the attacker's connection rate.
//
// This does mean a flood can fill the slots and keep legitimate clients out. That
// is the trade deliberately taken: SFTP being unreachable for a while is a far
// smaller failure than the game server being killed, and the handshake timeout
// keeps the slots turning over.
const defaultMaxPending = 256

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
	// HandshakeTimeout is how long a connection may take to authenticate before
	// it is dropped. Defaults to defaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// MaxPendingHandshakes caps concurrent unauthenticated connections; further
	// ones are closed immediately. Defaults to defaultMaxPending.
	MaxPendingHandshakes int
}

// Server is a key-only SFTP server.
type Server struct {
	cfg      Config
	sshConf  *ssh.ServerConfig
	listener net.Listener

	done  chan struct{}
	ready chan struct{} // closed once Serve has bound (or failed to bind)
	// pending is a counting semaphore over connections that have not finished
	// authenticating; a slot is released as soon as the handshake resolves.
	pending chan struct{}
	mu      sync.Mutex
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
	maxPending := cfg.MaxPendingHandshakes
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	return &Server{
		cfg:     cfg,
		sshConf: sc,
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		conns:   make(map[*ssh.ServerConn]string),
		pending: make(chan struct{}, maxPending),
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
		// Take a handshake slot before spawning anything, so a flood costs an
		// accept and a close rather than a goroutine apiece.
		select {
		case s.pending <- struct{}{}:
			go s.handleConn(conn)
		default:
			_ = conn.Close()
		}
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
	// The slot is held only for the handshake; an authenticated session gives it
	// back and is bounded by the key it holds instead.
	freed := false
	free := func() {
		if !freed {
			freed = true
			<-s.pending
		}
	}
	defer free()

	_ = c.SetDeadline(time.Now().Add(s.handshakeTimeout()))
	sconn, chans, reqs, err := ssh.NewServerConn(c, s.sshConf)
	if err != nil {
		return // failed handshake/auth
	}
	// Authenticated: an SFTP session is long-lived and legitimately idle between
	// operations, so the deadline that made sense for an anonymous connection
	// would now disconnect a working client mid-transfer.
	_ = c.SetDeadline(time.Time{})
	free()
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

// handshakeTimeout returns the configured pre-auth deadline, or the default.
func (s *Server) handshakeTimeout() time.Duration {
	if s.cfg.HandshakeTimeout > 0 {
		return s.cfg.HandshakeTimeout
	}
	return defaultHandshakeTimeout
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

// resolve maps a client path to a real path confined under base, as text.
func (r *root) resolve(p string) string {
	return filepath.Join(r.base, filepath.Clean("/"+p))
}

// safe is resolve plus symlink confinement. Textual confinement is not enough on
// its own: a symlink inside the volume points wherever it likes, and one can
// appear there without going through SFTP at all (an archive extracted from the
// panel, or the game process itself). Without this check such a link would hand
// a client the container's filesystem — including the server's SFTP host key —
// through a path that looks perfectly well-behaved.
//
// The deepest existing ancestor is resolved and must land inside the root. When
// deref is set the operation would follow the final component, so a symlink
// there is refused outright; when it is not (delete, rename) the link itself is
// the subject and is allowed, so a planted link can still be cleaned up.
func (r *root) safe(p string, deref bool) (string, error) {
	full := r.resolve(p)
	realRoot, err := filepath.EvalSymlinks(r.base)
	if err != nil {
		return "", err
	}
	probe := full
	if fi, lerr := os.Lstat(full); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if deref {
			return "", os.ErrPermission
		}
		probe = filepath.Dir(full)
	}
	// The leaf may not exist yet (a create); walk up to something that does.
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return "", os.ErrPermission
	}
	return full, nil
}

func (r *root) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	p, err := r.safe(req.Filepath, true)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_RDONLY, 0)
}

func (r *root) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	p, err := r.safe(req.Filepath, true)
	if err != nil {
		return nil, err
	}
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
	// Rename and Remove act on the entry itself and never follow it; the others
	// would dereference a symlink leaf, so they refuse one.
	deref := req.Method != "Rename" && req.Method != "Rmdir" && req.Method != "Remove"
	p, err := r.safe(req.Filepath, deref)
	if err != nil {
		return err
	}
	switch req.Method {
	case "Setstat":
		return r.setstat(p, req)
	case "Rename":
		to, err := r.safe(req.Target, false)
		if err != nil {
			return err
		}
		return os.Rename(p, to)
	case "Rmdir", "Remove":
		return os.Remove(p)
	case "Mkdir":
		return os.MkdirAll(p, 0o755)
	case "Symlink":
		// The link target is confined to the root, and safe() has already checked
		// that the link itself is being created inside it.
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
	// A Stat on a symlink is how a client discovers what it points at, and
	// listing one means descending into it: both dereference.
	p, err := r.safe(req.Filepath, true)
	if err != nil {
		return nil, err
	}
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
