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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/lolozini/quetzal/internal/authkeys"
	"github.com/lolozini/quetzal/internal/fileops"
)

// defaultRevokeInterval is how often live sessions are re-checked against the
// authorized keys so a revoked key is cut off, not just blocked at next connect.
//
// The keys reach the pod through a mounted ConfigMap, which the kubelet
// refreshes on its own clock, a minute or so after the change: new connections
// are refused from then on. Checking the open sessions every 30 seconds on top
// of that kept a revoked key's session alive up to two minutes after the click.
// Reading a file of a few keys every few seconds costs nothing, and closes it
// within seconds of when new connections are refused.
const defaultRevokeInterval = 5 * time.Second

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

// userExt carries the account the session signed in as, for the log.
const userExt = "quetzal-user"

// Config configures the server.
type Config struct {
	Addr    string // listen address, e.g. ":2022"
	Root    string // directory served as "/"
	HostKey []byte // PEM-encoded host private key
	// AuthorizedKeys returns the currently-authorized public keys, each with the
	// accounts it belongs to. It is called on every authentication attempt so
	// changes apply without a restart.
	AuthorizedKeys func() []authkeys.Key
	// LogOp, when set, is told of every change a session makes -- a write, a
	// removal, a rename, a new folder -- and who made it. Nothing said what
	// was done over SFTP, nor by whom.
	LogOp func(user, op, path string)
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
		// The name a session signs in as has to be one of the key's accounts:
		// any name went with any key, and the name a session showed said
		// nothing about whose key it was.
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			want := key.Marshal()
			for _, ak := range cfg.AuthorizedKeys() {
				if subtle.ConstantTimeCompare(want, ak.Key.Marshal()) != 1 {
					continue
				}
				if !ak.Allows(conn.User()) {
					return nil, fmt.Errorf("sshd: key not authorized for %q", conn.User())
				}
				return &ssh.Permissions{Extensions: map[string]string{
					pubkeyExt: base64.StdEncoding.EncodeToString(want),
					userExt:   conn.User(),
				}}, nil
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
		authorized[string(ak.Key.Marshal())] = true
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
	var blob, user string
	if sconn.Permissions != nil {
		if raw, err := base64.StdEncoding.DecodeString(sconn.Permissions.Extensions[pubkeyExt]); err == nil {
			blob = string(raw)
		}
		user = sconn.Permissions.Extensions[userExt]
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
		go s.serveSession(ch, chReqs, user)
	}
}

// handshakeTimeout returns the configured pre-auth deadline, or the default.
func (s *Server) handshakeTimeout() time.Duration {
	if s.cfg.HandshakeTimeout > 0 {
		return s.cfg.HandshakeTimeout
	}
	return defaultHandshakeTimeout
}

func (s *Server) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request, user string) {
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
	defer h.FileGet.(*root).Close()
	if h.FileGet.(*root).err != nil {
		_ = ch.Close()
		return
	}
	if s.cfg.LogOp != nil {
		h = loggedHandlers(h, func(op, p string) { s.cfg.LogOp(user, op, p) })
	}
	srv := sftp.NewRequestServer(ch, h)
	defer srv.Close()
	_ = srv.Serve()
}

// ---- rooted (chroot-like) handlers ----

// Each session holds the volume open. Path checks alone cannot protect against
// a game process replacing a checked parent or leaf before the operation.
type root struct {
	base string
	dir  *os.Root
	err  error
}

func rootedHandlers(base string) sftp.Handlers {
	dir, err := os.OpenRoot(base)
	r := &root{base: filepath.Clean(base), dir: dir, err: err}
	return sftp.Handlers{FileGet: r, FilePut: r, FileCmd: r, FileList: r}
}

func (r *root) Close() error {
	if r.dir != nil {
		return r.dir.Close()
	}
	return nil
}

func (r *root) resolve(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

func (r *root) safe(p string, deref bool) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return fileops.Resolve(r.dir, r.resolve(p), deref)
}

func (r *root) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	p, err := r.safe(req.Filepath, true)
	if err != nil {
		return nil, err
	}
	return r.dir.OpenFile(p, os.O_RDONLY, 0)
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
	return r.dir.OpenFile(p, flags, 0o644)
}

func (r *root) Filecmd(req *sftp.Request) error {
	if req.Method == "Symlink" {
		return r.symlink(req.Filepath, req.Target)
	}
	// Rename and removal operate on the entry itself, not its target.
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
		return r.dir.Rename(p, to)
	case "Rmdir", "Remove":
		return r.dir.Remove(p)
	case "Mkdir":
		return r.dir.MkdirAll(p, 0o755)
	default:
		return sftp.ErrSSHFxOpUnsupported
	}
}

// symlink creates link pointing at target, both as the client wrote them.
//
// OpenSSH sends a symlink's target before the link, the reverse of the draft it
// implements, and pkg/sftp hands them over in that order: Filepath is what the
// link points at, Target is the link. Read the other way round, `symlink /etc
// qa/sftplink` made a link named etc at the root, pointing at qa/sftplink.
//
// The link must be created inside the root, and what it points at is kept
// there too: an absolute target is taken from the root the client sees, a
// relative one from the link's directory, and ".." stops at the root either way.
func (r *root) symlink(target, link string) error {
	at, err := r.safe(link, false)
	if err != nil {
		return err
	}
	if !path.IsAbs(target) {
		target = path.Join(path.Dir(path.Clean("/"+link)), target)
	}
	to := r.resolve(target)
	if to == "" {
		to = "."
	}
	relative, err := filepath.Rel(filepath.Dir(at), to)
	if err != nil {
		return err
	}
	return r.dir.Symlink(relative, at)
}

func (r *root) setstat(p string, req *sftp.Request) error {
	attr := req.Attributes()
	if !req.AttrFlags().Size {
		if req.AttrFlags().Permissions {
			// Chmod must also work on a file the owner cannot read. Root.Chmod
			// uses a pinned parent and AT_SYMLINK_NOFOLLOW on Linux: a swap
			// can affect the link itself, never a target outside the root.
			return r.dir.Chmod(p, attr.FileMode())
		}
		return nil
	}
	f, err := r.dir.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(int64(attr.Size)); err != nil {
		return err
	}
	if req.AttrFlags().Permissions {
		if err := f.Chmod(attr.FileMode()); err != nil {
			return err
		}
	}
	return nil
}

func (r *root) Filelist(req *sftp.Request) (sftp.ListerAt, error) {
	// Directory and file metadata are obtained through the held root.
	p, err := r.safe(req.Filepath, true)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "List":
		f, err := r.dir.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		infos, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		return listerat(infos), nil
	case "Stat":
		fi, err := r.dir.Stat(p)
		if err != nil {
			return nil, err
		}
		return listerat{fi}, nil
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
}

func (r *root) Lstat(req *sftp.Request) (sftp.ListerAt, error) {
	p, err := r.safe(req.Filepath, false)
	if err != nil {
		return nil, err
	}
	fi, err := r.dir.Lstat(p)
	if err != nil {
		return nil, err
	}
	return listerat{fi}, nil
}

func (r *root) Readlink(name string) (string, error) {
	p, err := r.safe(name, false)
	if err != nil {
		return "", err
	}
	target, err := r.dir.Readlink(p)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(r.base, target)
		if err == nil && filepath.IsLocal(rel) {
			return "/" + filepath.ToSlash(rel), nil
		}
	}
	return target, nil
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

// ---- the log of what sessions change ----

// loggedHandlers tells logOp of each change a session makes, then makes it.
func loggedHandlers(h sftp.Handlers, logOp func(op, path string)) sftp.Handlers {
	return sftp.Handlers{
		FileGet:  h.FileGet,
		FileList: h.FileList,
		FilePut:  loggedPut{h.FilePut, logOp},
		FileCmd:  loggedCmd{h.FileCmd, logOp},
	}
}

type loggedPut struct {
	sftp.FileWriter
	logOp func(op, path string)
}

func (l loggedPut) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	w, err := l.FileWriter.Filewrite(req)
	if err == nil {
		l.logOp("write", req.Filepath)
	}
	return w, err
}

type loggedCmd struct {
	sftp.FileCmder
	logOp func(op, path string)
}

func (l loggedCmd) Filecmd(req *sftp.Request) error {
	err := l.FileCmder.Filecmd(req)
	if err == nil {
		switch req.Method {
		case "Rename":
			l.logOp("rename", req.Filepath+" -> "+req.Target)
		case "Symlink":
			// pkg/sftp hands OpenSSH's order over: Filepath is the target.
			l.logOp("symlink", req.Target+" -> "+req.Filepath)
		default:
			l.logOp(strings.ToLower(req.Method), req.Filepath)
		}
	}
	return err
}
