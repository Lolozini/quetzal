package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// ---- setup & auth ----

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needed": n == 0})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "already configured")
		return
	}
	var req credentials
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "username >=3 and password >=8 chars required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	u := &models.User{Username: req.Username, PasswordHash: hash, IsAdmin: true}
	if err := s.Store.CreateUser(u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.startSession(w, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	u, err := s.Store.GetUserByUsername(req.Username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	ok, err := auth.VerifyPassword(u.PasswordHash, req.Password)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err := s.startSession(w, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := tokenFromRequest(r); token != "" {
		_ = s.Store.DeleteSession(token)
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userFrom(r.Context()))
}

func (s *Server) startSession(w http.ResponseWriter, u *models.User) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	exp := time.Now().Add(s.SessionTTL)
	if err := s.Store.CreateSession(&models.Session{Token: token, UserID: u.ID, ExpiresAt: exp}); err != nil {
		return err
	}
	s.setSessionCookie(w, token, exp)
	return nil
}

// ---- templates ----

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListTemplates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// ---- servers ----

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	srvs, err := s.Store.ListServers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, srvs)
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

type createServerRequest struct {
	Name     string            `json:"name"`
	Template string            `json:"template"`
	Image    string            `json:"image"`
	Memory   string            `json:"memory"`
	CPU      string            `json:"cpu"`
	Storage  models.Storage    `json:"storage"`
	Env      map[string]string `json:"env"`
	Start    bool              `json:"start"`
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var req createServerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Name == "" || req.Template == "" {
		writeError(w, http.StatusBadRequest, "name and template are required")
		return
	}
	tmpl, err := s.Store.GetTemplateBySlug(req.Template)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown template")
		return
	}
	slug := egg.Slugify(req.Name)
	if slug == "" {
		writeError(w, http.StatusBadRequest, "name produces an empty slug")
		return
	}
	if _, err := s.Store.GetServerBySlug(slug); err == nil {
		writeError(w, http.StatusConflict, "a server with this name already exists")
		return
	}

	image := req.Image
	if image == "" {
		image = defaultImage(tmpl)
	}

	env := map[string]string{}
	for _, v := range tmpl.Variables {
		if v.Default != "" {
			env[v.EnvVariable] = v.Default
		}
	}
	for k, v := range req.Env {
		env[k] = v
	}

	// Split sensitive values out of the clear-text env: they are encrypted and
	// materialized into a Kubernetes Secret by the controller.
	secretNames := map[string]bool{}
	for _, v := range tmpl.Variables {
		if v.Secret {
			secretNames[v.EnvVariable] = true
		}
	}
	plainEnv := map[string]string{}
	secretEnv := map[string]string{}
	for k, v := range env {
		if secretNames[k] {
			secretEnv[k] = v
		} else {
			plainEnv[k] = v
		}
	}
	sealed, err := s.Store.SealSecrets(secretEnv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to seal secrets")
		return
	}

	storage := req.Storage
	if storage.Type == "" {
		storage.Type = models.StoragePVC
	}
	if storage.Type == models.StoragePVC && storage.Size == "" {
		storage.Size = "10Gi"
	}
	if storage.Type == models.StorageHostPath && storage.HostPath == "" {
		writeError(w, http.StatusBadRequest, "hostPath storage requires a path")
		return
	}

	state := models.StateStopped
	if req.Start {
		state = models.StateRunning
	}

	srv := &models.Server{
		Slug:            slug,
		DisplayName:     req.Name,
		OwnerID:         userFrom(r.Context()).ID,
		TemplateID:      tmpl.ID,
		TemplateVersion: tmpl.Version,
		Image:           image,
		Namespace:       reconciler.NamespaceFor(slug),
		DesiredState:    state,
		Resources:       models.Resources{Memory: req.Memory, CPU: req.CPU},
		Env:             plainEnv,
		SecretEnvEnc:    sealed,
		Storage:         storage,
		Ports:           tmpl.Ports,
		Status:          models.Status{Phase: models.PhaseStopped},
	}
	if err := s.Store.CreateServer(srv); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, srv)
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteServer(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type powerRequest struct {
	Action string `json:"action"` // start | stop | restart | kill
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	var req powerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	switch req.Action {
	case "start":
		srv.DesiredState = models.StateRunning
		if err := s.Store.UpdateServer(srv); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "stop":
		srv.DesiredState = models.StateStopped
		if err := s.Store.UpdateServer(srv); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "restart":
		if err := s.deletePods(r, srv, nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "kill":
		zero := int64(0)
		if err := s.deletePods(r, srv, &zero); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action must be start|stop|restart|kill")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"action": req.Action, "result": "accepted"})
}

func (s *Server) deletePods(r *http.Request, srv *models.Server, grace *int64) error {
	return s.Clientset.CoreV1().Pods(srv.Namespace).DeleteCollection(
		r.Context(),
		metav1.DeleteOptions{GracePeriodSeconds: grace},
		metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + srv.Slug},
	)
}

// ---- console ----

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	// Resolve the pod before upgrading so we can still return a JSON error.
	pod, err := console.FindRunningPod(r.Context(), s.Clientset, srv.Namespace, srv.Slug)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the response
	}
	defer conn.Close()
	_ = console.Stream(r.Context(), conn, s.Clientset, s.RestConfig, srv.Namespace, pod)
}

// ---- helpers ----

func (s *Server) lookupServer(w http.ResponseWriter, r *http.Request) (*models.Server, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid server id")
		return nil, false
	}
	srv, err := s.Store.GetServer(uint(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return srv, true
}

func defaultImage(t *models.Template) string {
	for _, img := range t.Images {
		if img.Default {
			return img.Ref
		}
	}
	if len(t.Images) > 0 {
		return t.Images[0].Ref
	}
	return ""
}
