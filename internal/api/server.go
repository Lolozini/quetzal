// Package api is the Quetzal HTTP API: auth, server CRUD/power, and the live
// console WebSocket. The database is the source of truth; power actions update
// desired state which the controller reconciles.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/ratelimit"
	"github.com/lolozini/quetzal/internal/store"
)

const sessionCookie = "quetzal_session"

// Server holds the API dependencies.
type Server struct {
	Store      *store.Store
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
	// Registry resolves per-cluster k8s clients (the passed-in clientset is the
	// local cluster). Server-scoped handlers route to the server's own cluster.
	Registry *cluster.Registry
	// SessionTTL controls how long a login lasts.
	SessionTTL time.Duration
	// Secure marks cookies Secure (set when served over HTTPS).
	Secure bool
	// NodePortMin/NodePortMax bound the control-plane node port pool (0 = use
	// the Kubernetes default range 30000-32767).
	NodePortMin int32
	NodePortMax int32
	// WakeKey signs/verifies per-server wake-on-connect callback tokens (shared
	// with the controller via QUETZAL_SECRET_KEY).
	WakeKey []byte
	// Dispatch delivers events to notification channels. When set, emit() nudges
	// it for prompt delivery; nil disables notifications (events are still
	// recorded for the activity feed).
	Dispatch *notify.Dispatcher

	// Rate limiters (per-process). LoginLimiter is keyed by username, AuthIPLimiter
	// and InternalLimiter by client IP, ForgotLimiter by reset identifier.
	// Replaceable in tests.
	LoginLimiter    *ratelimit.Limiter
	AuthIPLimiter   *ratelimit.Limiter
	InternalLimiter *ratelimit.Limiter
	ForgotLimiter   *ratelimit.Limiter
	// Mailer sends outbound system email (password reset). Defaults to
	// notify.SendMail; overridable in tests.
	Mailer MailSender
	// TrustProxy honors X-Forwarded-For when deriving the client IP (set when
	// served behind a reverse proxy such as Traefik).
	TrustProxy bool

	upgrader websocket.Upgrader
}

// New builds an API server.
func New(st *store.Store, cs kubernetes.Interface, cfg *rest.Config) *Server {
	s := &Server{
		Store:      st,
		Clientset:  cs,
		RestConfig: cfg,
		Registry:   cluster.New(st, cluster.Clients{Clientset: cs, Config: cfg}),
		SessionTTL: 7 * 24 * time.Hour,
		// Brute-force defaults: 10 login attempts / 15 min per username (covers
		// password and TOTP-code guessing), a broader per-IP cap, and a generous
		// cap on the in-cluster wake/active callbacks.
		LoginLimiter:    ratelimit.New(10, 15*time.Minute),
		AuthIPLimiter:   ratelimit.New(60, 15*time.Minute),
		InternalLimiter: ratelimit.New(120, time.Minute),
		// Password-reset requests: cap per identifier to avoid emailing-bombing a
		// victim and to blunt account enumeration via repeated probing.
		ForgotLimiter: ratelimit.New(3, time.Hour),
		Mailer:        notify.SendMail,
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: s.checkOrigin}
	return s
}

// MailSender sends a plain-text email; see notify.SendMail.
type MailSender func(ctx context.Context, cfg map[string]string, to []string, subject, body string) error

// GCRateLimiters drops expired counters from all limiters; call periodically.
func (s *Server) GCRateLimiters() {
	s.LoginLimiter.GC()
	s.AuthIPLimiter.GC()
	s.InternalLimiter.GC()
	s.ForgotLimiter.GC()
}

// clientIP returns the caller's IP, honoring X-Forwarded-For only when behind a
// trusted proxy (it is attacker-spoofable otherwise). When trusted, it takes the
// RIGHTMOST entry: a proxy that appends (Traefik, nginx) puts the real client
// last, so any client-supplied X-Forwarded-For sits to the left and is ignored.
// This assumes a single trusted proxy hop.
func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[i+1:])
			}
			return strings.TrimSpace(xff)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func tooManyRequests(w http.ResponseWriter, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeError(w, http.StatusTooManyRequests, "too many requests, slow down")
}

// clientsFor returns the Kubernetes clientset + rest config for a server's
// target cluster.
func (s *Server) clientsFor(srv *models.Server) (kubernetes.Interface, *rest.Config, error) {
	c, err := s.Registry.For(srv.ClusterID)
	if err != nil {
		return nil, nil, err
	}
	return c.Clientset, c.Config, nil
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public (no auth).
	mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	// Self-service password reset (rate-limited; always responds uniformly).
	mux.HandleFunc("POST /api/forgot-password", s.handleForgotPassword)
	mux.HandleFunc("POST /api/reset-password", s.handleResetPassword)
	// Wake-on-connect callbacks from a server's activator (token-authenticated,
	// no session). Reachable in-cluster from server namespaces.
	mux.HandleFunc("POST /api/internal/wake", s.handleWake)
	mux.HandleFunc("POST /api/internal/active", s.handleActive)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Public API documentation: the machine-readable spec and a rendered viewer.
	mux.HandleFunc("GET /api/openapi.yaml", s.handleOpenAPISpec)
	mux.HandleFunc("GET /api/docs", s.handleDocs)

	// Protected.
	mux.Handle("GET /api/me", s.auth(s.handleMe))
	mux.Handle("PUT /api/me/email", s.auth(s.handleSetMyEmail))
	mux.Handle("GET /api/templates", s.auth(s.handleListTemplates))
	mux.Handle("GET /api/templates/{slug}", s.auth(s.handleGetTemplate))
	mux.Handle("POST /api/templates/import", s.auth(s.handleImportEgg))
	mux.Handle("PUT /api/templates/{slug}", s.auth(s.handleUpdateTemplate))
	mux.Handle("DELETE /api/templates/{slug}", s.auth(s.handleDeleteTemplate))
	mux.Handle("GET /api/templates/{slug}/export", s.auth(s.handleExportTemplate))
	mux.Handle("GET /api/servers", s.auth(s.handleListServers))
	mux.Handle("POST /api/servers", s.auth(s.handleCreateServer))
	mux.Handle("GET /api/servers/{id}", s.auth(s.handleGetServer))
	mux.Handle("PATCH /api/servers/{id}", s.auth(s.handleUpdateServer))
	mux.Handle("DELETE /api/servers/{id}", s.auth(s.handleDeleteServer))
	mux.Handle("POST /api/servers/{id}/power", s.auth(s.handlePower))
	mux.Handle("POST /api/servers/{id}/reinstall", s.auth(s.handleReinstallServer))
	mux.Handle("GET /api/servers/{id}/stats", s.auth(s.handleServerStats))
	mux.Handle("GET /api/servers/{id}/console", s.auth(s.handleConsole))
	// File manager (exec into the running pod; requires the files permission).
	mux.Handle("GET /api/servers/{id}/files", s.auth(s.handleListFiles))
	mux.Handle("GET /api/servers/{id}/files/content", s.auth(s.handleReadFile))
	mux.Handle("GET /api/servers/{id}/files/archive", s.auth(s.handleArchiveFile))
	mux.Handle("PUT /api/servers/{id}/files/content", s.auth(s.handleWriteFile))
	mux.Handle("POST /api/servers/{id}/files/extract", s.auth(s.handleExtractArchive))
	mux.Handle("POST /api/servers/{id}/files/mkdir", s.auth(s.handleMkdir))
	mux.Handle("POST /api/servers/{id}/files/rename", s.auth(s.handleRenameFile))
	mux.Handle("DELETE /api/servers/{id}/files", s.auth(s.handleDeleteFile))
	mux.Handle("GET /api/servers/{id}/schedules", s.auth(s.handleListSchedules))
	mux.Handle("POST /api/servers/{id}/schedules", s.auth(s.handleCreateSchedule))
	mux.Handle("PATCH /api/servers/{id}/schedules/{sid}", s.auth(s.handleUpdateSchedule))
	mux.Handle("DELETE /api/servers/{id}/schedules/{sid}", s.auth(s.handleDeleteSchedule))

	// Backups.
	mux.Handle("GET /api/backup-config", s.auth(s.handleGetBackupConfig))
	mux.Handle("PUT /api/backup-config", s.auth(s.handleSetBackupConfig))
	mux.Handle("GET /api/servers/{id}/backups", s.auth(s.handleListBackups))
	mux.Handle("POST /api/servers/{id}/backups", s.auth(s.handleCreateBackup))
	mux.Handle("POST /api/servers/{id}/backups/{bid}/restore", s.auth(s.handleRestoreBackup))
	mux.Handle("DELETE /api/servers/{id}/backups/{bid}", s.auth(s.handleDeleteBackup))

	// Multi-tenant: suspend (admin), subuser access, audit.
	mux.Handle("POST /api/servers/{id}/suspend", s.auth(s.handleSuspend))
	mux.Handle("POST /api/servers/{id}/unsuspend", s.auth(s.handleUnsuspend))
	mux.Handle("GET /api/servers/{id}/access", s.auth(s.handleListAccess))
	mux.Handle("POST /api/servers/{id}/access", s.auth(s.handleGrantAccess))
	mux.Handle("DELETE /api/servers/{id}/access/{uid}", s.auth(s.handleRevokeAccess))
	mux.Handle("GET /api/servers/{id}/audit", s.auth(s.handleServerAudit))
	mux.Handle("GET /api/audit", s.auth(s.handleGlobalAudit))

	// Users (admin) + self password change.
	mux.Handle("GET /api/users", s.auth(s.handleListUsers))
	mux.Handle("POST /api/users", s.auth(s.handleCreateUser))
	mux.Handle("PATCH /api/users/{uid}", s.auth(s.handleUpdateUser))
	mux.Handle("DELETE /api/users/{uid}", s.auth(s.handleDeleteUser))
	mux.Handle("PUT /api/users/{uid}/admin-role", s.auth(s.handleSetUserAdminRole))
	mux.Handle("POST /api/me/password", s.auth(s.handleChangePassword))

	// Admin roles (scoped admin permission bundles; superadmin only).
	mux.Handle("GET /api/admin-permissions", s.auth(s.handleAdminPermissionCatalog))
	mux.Handle("GET /api/admin-roles", s.auth(s.handleListAdminRoles))
	mux.Handle("POST /api/admin-roles", s.auth(s.handleCreateAdminRole))
	mux.Handle("PUT /api/admin-roles/{rid}", s.auth(s.handleUpdateAdminRole))
	mux.Handle("DELETE /api/admin-roles/{rid}", s.auth(s.handleDeleteAdminRole))

	// System email settings (admin) — drives password-reset delivery.
	mux.Handle("GET /api/email-settings", s.auth(s.handleGetEmailSettings))
	mux.Handle("PUT /api/email-settings", s.auth(s.handleSetEmailSettings))
	mux.Handle("POST /api/email-settings/test", s.auth(s.handleTestEmail))

	// Database hosts (admin registry) + per-server databases.
	mux.Handle("GET /api/database-hosts", s.auth(s.handleListDatabaseHosts))
	mux.Handle("POST /api/database-hosts", s.auth(s.handleCreateDatabaseHost))
	mux.Handle("PATCH /api/database-hosts/{hid}", s.auth(s.handleUpdateDatabaseHost))
	mux.Handle("DELETE /api/database-hosts/{hid}", s.auth(s.handleDeleteDatabaseHost))
	mux.Handle("POST /api/database-hosts/{hid}/test", s.auth(s.handleTestDatabaseHost))
	mux.Handle("GET /api/servers/{id}/database-hosts", s.auth(s.handleListServerDatabaseHosts))
	mux.Handle("GET /api/servers/{id}/databases", s.auth(s.handleListServerDatabases))
	mux.Handle("POST /api/servers/{id}/databases", s.auth(s.handleCreateServerDatabase))
	mux.Handle("GET /api/servers/{id}/databases/{dbid}", s.auth(s.handleGetServerDatabase))
	mux.Handle("POST /api/servers/{id}/databases/{dbid}/rotate", s.auth(s.handleRotateServerDatabase))
	mux.Handle("DELETE /api/servers/{id}/databases/{dbid}", s.auth(s.handleDeleteServerDatabase))

	// Two-factor authentication (opt-in TOTP) for the current user, plus an
	// admin reset for the lost-device lockout case.
	mux.Handle("GET /api/me/sshkeys", s.auth(s.handleListSSHKeys))
	mux.Handle("POST /api/me/sshkeys", s.auth(s.handleAddSSHKey))
	mux.Handle("DELETE /api/me/sshkeys/{kid}", s.auth(s.handleDeleteSSHKey))
	mux.Handle("GET /api/servers/{id}/sftp", s.auth(s.handleServerSFTP))
	mux.Handle("POST /api/me/2fa/setup", s.auth(s.handle2FASetup))
	mux.Handle("POST /api/me/2fa/enable", s.auth(s.handle2FAEnable))
	mux.Handle("POST /api/me/2fa/disable", s.auth(s.handle2FADisable))
	mux.Handle("POST /api/users/{uid}/2fa/disable", s.auth(s.handleAdminDisable2FA))

	// API keys (scoped bearer tokens for the public API).
	mux.Handle("GET /api/apikeys", s.auth(s.handleListAPIKeys))
	mux.Handle("POST /api/apikeys", s.auth(s.handleCreateAPIKey))
	mux.Handle("DELETE /api/apikeys/{kid}", s.auth(s.handleDeleteAPIKey))

	// Clusters (multi-cluster registry). Listing is open to any authenticated
	// user (to pick a deploy target); mutations are admin-only.
	mux.Handle("GET /api/clusters", s.auth(s.handleListClusters))
	mux.Handle("POST /api/clusters", s.auth(s.handleCreateCluster))
	mux.Handle("PATCH /api/clusters/{cid}", s.auth(s.handleUpdateCluster))
	mux.Handle("DELETE /api/clusters/{cid}", s.auth(s.handleDeleteCluster))
	mux.Handle("POST /api/clusters/{cid}/test", s.auth(s.handleTestCluster))
	mux.Handle("GET /api/clusters/{cid}/nodes", s.auth(s.handleClusterNodes))

	// Notification channels (Discord/webhook/email) + activity feed. Global
	// channels are admin-only; server-scoped ones need PermSettings on the server.
	mux.Handle("GET /api/notifications/channels", s.auth(s.handleListChannels))
	mux.Handle("POST /api/notifications/channels", s.auth(s.handleCreateChannel))
	mux.Handle("GET /api/notifications/channels/{nid}", s.auth(s.handleGetChannel))
	mux.Handle("PATCH /api/notifications/channels/{nid}", s.auth(s.handleUpdateChannel))
	mux.Handle("DELETE /api/notifications/channels/{nid}", s.auth(s.handleDeleteChannel))
	mux.Handle("POST /api/notifications/channels/{nid}/test", s.auth(s.handleTestChannel))
	mux.Handle("GET /api/servers/{id}/notifications", s.auth(s.handleListServerChannels))
	mux.Handle("GET /api/servers/{id}/events", s.auth(s.handleServerEvents))
	mux.Handle("GET /api/events", s.auth(s.handleGlobalEvents))

	return logRequests(s.csrf(mux))
}

// csrf blocks state-changing requests whose Origin/Referer is cross-origin. The
// session cookie is SameSite=Lax (already blocking most cross-site sends); this
// is defense-in-depth. Non-browser clients send neither header and authenticate
// with bearer tokens (not ambient cookies), so they are unaffected.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe, no state change
		default:
			if !s.sameOriginRequest(r) {
				writeError(w, http.StatusForbidden, "cross-origin request blocked")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginRequest reports whether an unsafe request's Origin (or, failing that,
// Referer) matches the request Host. A request with neither header is allowed.
func (s *Server) sameOriginRequest(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

// ---- auth middleware & helpers ----

type ctxKey string

const userCtxKey ctxKey = "user"

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUser(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, user)
		next(w, r.WithContext(ctx))
	})
}

func userFrom(ctx context.Context) *models.User {
	u, _ := ctx.Value(userCtxKey).(*models.User)
	return u
}

func (s *Server) currentUser(r *http.Request) (*models.User, error) {
	token := tokenFromRequest(r)
	if token == "" {
		return nil, store.ErrNotFound
	}
	// API keys (long-lived bearer tokens) are identified by their prefix.
	if strings.HasPrefix(token, apiKeyPrefix) {
		key, err := s.Store.GetAPIKeyByHash(hashToken(token))
		if err != nil {
			return nil, err
		}
		now := time.Now()
		_ = s.Store.TouchAPIKey(key.ID, now)
		return s.Store.GetUser(key.UserID)
	}
	sess, err := s.Store.GetSession(token)
	if err != nil {
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.Store.DeleteSession(token)
		return nil, store.ErrNotFound
	}
	return s.Store.GetUser(sess.UserID)
}

func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// checkOrigin permits same-origin and localhost (dev) WebSocket upgrades.
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1"
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// statusWriter records the response status while preserving Hijacker (needed
// for the console WebSocket upgrade) and Flusher.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}
