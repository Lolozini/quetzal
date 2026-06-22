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
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/notify"
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
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: s.checkOrigin}
	return s
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
	// Wake-on-connect callbacks from a server's activator (token-authenticated,
	// no session). Reachable in-cluster from server namespaces.
	mux.HandleFunc("POST /api/internal/wake", s.handleWake)
	mux.HandleFunc("POST /api/internal/active", s.handleActive)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Protected.
	mux.Handle("GET /api/me", s.auth(s.handleMe))
	mux.Handle("GET /api/templates", s.auth(s.handleListTemplates))
	mux.Handle("GET /api/servers", s.auth(s.handleListServers))
	mux.Handle("POST /api/servers", s.auth(s.handleCreateServer))
	mux.Handle("GET /api/servers/{id}", s.auth(s.handleGetServer))
	mux.Handle("PATCH /api/servers/{id}", s.auth(s.handleUpdateServer))
	mux.Handle("DELETE /api/servers/{id}", s.auth(s.handleDeleteServer))
	mux.Handle("POST /api/servers/{id}/power", s.auth(s.handlePower))
	mux.Handle("GET /api/servers/{id}/stats", s.auth(s.handleServerStats))
	mux.Handle("GET /api/servers/{id}/console", s.auth(s.handleConsole))
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
	mux.Handle("POST /api/me/password", s.auth(s.handleChangePassword))

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

	return logRequests(mux)
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
