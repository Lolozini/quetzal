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

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

const sessionCookie = "quetzal_session"

// Server holds the API dependencies.
type Server struct {
	Store      *store.Store
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
	// SessionTTL controls how long a login lasts.
	SessionTTL time.Duration
	// Secure marks cookies Secure (set when served over HTTPS).
	Secure bool

	upgrader websocket.Upgrader
}

// New builds an API server.
func New(st *store.Store, cs kubernetes.Interface, cfg *rest.Config) *Server {
	s := &Server{
		Store:      st,
		Clientset:  cs,
		RestConfig: cfg,
		SessionTTL: 7 * 24 * time.Hour,
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: s.checkOrigin}
	return s
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public (no auth).
	mux.HandleFunc("GET /api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Protected.
	mux.Handle("GET /api/me", s.auth(s.handleMe))
	mux.Handle("GET /api/templates", s.auth(s.handleListTemplates))
	mux.Handle("GET /api/servers", s.auth(s.handleListServers))
	mux.Handle("POST /api/servers", s.auth(s.handleCreateServer))
	mux.Handle("GET /api/servers/{id}", s.auth(s.handleGetServer))
	mux.Handle("DELETE /api/servers/{id}", s.auth(s.handleDeleteServer))
	mux.Handle("POST /api/servers/{id}/power", s.auth(s.handlePower))
	mux.Handle("GET /api/servers/{id}/console", s.auth(s.handleConsole))

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
