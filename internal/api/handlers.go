package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/egg"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/stats"
	"github.com/lolozini/quetzal/internal/store"
)

// maxServerSlugLen keeps "quetzal-srv-<slug>" within the 63-char namespace limit.
const maxServerSlugLen = 50

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
	Expose   models.Expose     `json:"expose"`
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
	// Cap the slug so the per-server namespace ("quetzal-srv-<slug>") stays within
	// the 63-char DNS-1123 limit.
	if len(slug) > maxServerSlugLen {
		slug = strings.TrimRight(slug[:maxServerSlugLen], "-")
	}
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

	if err := validateExpose(req.Expose, len(tmpl.Ports) > 0); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
		Expose:          req.Expose,
		Status:          models.Status{Phase: models.PhaseStopped},
	}
	if err := s.Store.CreateServer(srv); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// NodePort exposure draws stable ports from the control-plane pool, which
	// needs the server's ID, so allocate after the row exists.
	if req.Expose.ServiceType() == models.ExposeNodePort {
		ports, err := s.allocateNodePorts(srv.ID, tmpl.Ports)
		if err != nil {
			_ = s.Store.DeleteServer(srv.ID) // avoid a half-configured record
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err := s.Store.UpdateServerNetworking(srv.ID, req.Expose, ports); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		srv.Ports = ports
	}
	writeJSON(w, http.StatusCreated, srv)
}

type updateServerRequest struct {
	// Expose, when present, reconfigures external reachability (and reallocates
	// or frees pool node ports accordingly).
	Expose *models.Expose `json:"expose"`
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	var req updateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Expose == nil {
		writeJSON(w, http.StatusOK, srv) // nothing to change
		return
	}
	expose := *req.Expose
	if err := validateExpose(expose, len(srv.Ports) > 0); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var ports []models.PortSpec
	if expose.ServiceType() == models.ExposeNodePort {
		var err error
		if ports, err = s.allocateNodePorts(srv.ID, srv.Ports); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	} else {
		if err := s.Store.ReleaseServerPorts(srv.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ports = clearNodePorts(srv.Ports)
	}
	if err := s.Store.UpdateServerNetworking(srv.ID, expose, ports); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	srv.Expose = expose
	srv.Ports = ports
	writeJSON(w, http.StatusOK, srv)
}

// allocateNodePorts reserves a stable pool node port for each of a server's
// ports, returning the port list with NodePort fields set.
func (s *Server) allocateNodePorts(serverID uint, ports []models.PortSpec) ([]models.PortSpec, error) {
	out := make([]models.PortSpec, len(ports))
	copy(out, ports)
	for i := range out {
		name := out[i].Name
		if name == "" {
			name = fmt.Sprintf("p%d", out[i].Port)
		}
		np, err := s.Store.AllocateNodePort(serverID, name, s.NodePortMin, s.NodePortMax)
		if err != nil {
			return nil, err
		}
		out[i].NodePort = np
	}
	return out, nil
}

func clearNodePorts(ports []models.PortSpec) []models.PortSpec {
	out := make([]models.PortSpec, len(ports))
	copy(out, ports)
	for i := range out {
		out[i].NodePort = 0
	}
	return out
}

func validateExpose(e models.Expose, hasPorts bool) error {
	switch e.ServiceType() {
	case models.ExposeClusterIP, models.ExposeNodePort, models.ExposeLoadBalancer:
	default:
		return fmt.Errorf("invalid expose type %q", e.Type)
	}
	if e.External() && !hasPorts {
		return errors.New("cannot publish a server that declares no ports")
	}
	return nil
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	// keepData decides the data lifecycle. The query param (sent by the UI's
	// delete dialog) wins; otherwise fall back to the server's stored policy.
	keep := srv.Storage.RetainOnDelete
	if q := r.URL.Query().Get("keepData"); q != "" {
		keep = q == "true"
	}
	if err := s.teardownServer(r.Context(), srv, keep); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.DeleteSchedulesForServer(srv.ID)
	_ = s.Store.DeleteBackupsForServer(srv.ID)
	if err := s.Store.DeleteServer(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// teardownServer removes a server's cluster resources by deleting its namespace.
// When keepData is set and the server uses a PVC, the bound PersistentVolume's
// reclaim policy is switched to Retain first, so the underlying volume (and its
// data) survives the namespace/PVC deletion as a Released PV. hostPath data is
// inherently retained (deleting the namespace never touches the node path).
func (s *Server) teardownServer(ctx context.Context, srv *models.Server, keepData bool) error {
	if keepData && srv.Storage.Type == models.StoragePVC {
		if pv := s.boundPV(ctx, srv.Namespace); pv != "" {
			patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`)
			if _, err := s.Clientset.CoreV1().PersistentVolumes().Patch(ctx, pv, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("retain volume %s: %w", pv, err)
			}
			// Surface the retained PV so the operator can recover/rebind it later.
			log.Printf("server %s deleted with keepData: retained PersistentVolume %q (now Released)", srv.Slug, pv)
		}
	}
	err := s.Clientset.CoreV1().Namespaces().Delete(ctx, srv.Namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// boundPV returns the PersistentVolume backing a server's data PVC, or "".
func (s *Server) boundPV(ctx context.Context, ns string) string {
	pvc, err := s.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, reconciler.DataVolume, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvc.Spec.VolumeName
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
		if err := s.Store.SetDesiredState(srv.ID, models.StateRunning); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "stop":
		if err := s.Store.SetDesiredState(srv.ID, models.StateStopped); err != nil {
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
	// A console only makes sense for a running server; otherwise there is no
	// (and will be no) pod to attach to. The stream itself tolerates a pod that
	// is still starting or crash-looping.
	if srv.DesiredState != models.StateRunning {
		writeError(w, http.StatusConflict, "server is not running")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the response
	}
	defer conn.Close()
	_ = console.Stream(r.Context(), conn, s.Clientset, s.RestConfig, srv.Namespace, srv.Slug)
}

// ---- observability ----

func (s *Server) handleServerStats(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	pod, err := console.FindRunningPod(r.Context(), s.Clientset, srv.Namespace, srv.Slug)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	u, err := stats.PodUsage(r.Context(), s.Clientset, srv.Namespace, pod)
	if err != nil {
		if errors.Is(err, stats.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cpuMillicores": u.CPUMillicores,
		"memoryBytes":   u.MemoryBytes,
		"cpuLimit":      srv.Resources.CPU,
		"memoryLimit":   srv.Resources.Memory,
	})
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
