package api

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/lolozini/quetzal/internal/auth"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/crypto"
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
	// Code is an optional TOTP or recovery code, supplied on the second step of
	// login when the account has two-factor authentication enabled.
	Code string `json:"code"`
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
	ip := s.clientIP(r)
	if !s.AuthIPLimiter.Allow(ip) {
		tooManyRequests(w, s.AuthIPLimiter.RetryAfter(ip))
		return
	}
	var req credentials
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	// Per-account brute-force throttle (bounds password and TOTP-code guessing);
	// cleared on a fully successful login below.
	userKey := strings.ToLower(strings.TrimSpace(req.Username))
	if !s.LoginLimiter.Allow(userKey) {
		tooManyRequests(w, s.LoginLimiter.RetryAfter(userKey))
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
	// Second factor: when 2FA is enabled, the password alone yields no session.
	// A first request without a code gets a challenge; the client resubmits with
	// a TOTP or recovery code.
	if u.TOTPEnabled {
		if strings.TrimSpace(req.Code) == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"twoFactorRequired": true})
			return
		}
		if !s.verifyTwoFactor(u, req.Code) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "invalid two-factor code", "twoFactorRequired": true,
			})
			return
		}
	}
	if err := s.startSession(w, u); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Successful login: don't hold earlier failures against this user/IP.
	s.LoginLimiter.Reset(userKey)
	s.AuthIPLimiter.Reset(ip)
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
	u := userFrom(r.Context())
	var srvs []models.Server
	var err error
	if u.IsAdmin {
		srvs, err = s.Store.ListServers()
	} else {
		srvs, err = s.Store.ListAccessibleServers(u.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, srvs)
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

type createServerRequest struct {
	Name        string             `json:"name"`
	Template    string             `json:"template"`
	Image       string             `json:"image"`
	Memory      string             `json:"memory"`
	CPU         string             `json:"cpu"`
	Storage     models.Storage     `json:"storage"`
	Env         map[string]string  `json:"env"`
	Expose      models.Expose      `json:"expose"`
	Hibernation models.Hibernation `json:"hibernation"`
	Cluster     string             `json:"cluster"` // target cluster slug ("" = local)
	Start       bool               `json:"start"`
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

	env, err := resolveEnv(tmpl, req.Env)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

	// Resolve the target cluster (default: the local / in-cluster cluster).
	clusterID, err := s.resolveCluster(req.Cluster)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	owner := userFrom(r.Context())
	if err := s.checkQuota(owner, req.Memory, req.CPU); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	state := models.StateStopped
	if req.Start {
		state = models.StateRunning
	}

	srv := &models.Server{
		Slug:            slug,
		DisplayName:     req.Name,
		OwnerID:         owner.ID,
		TemplateID:      tmpl.ID,
		TemplateVersion: tmpl.Version,
		Image:           image,
		Namespace:       reconciler.NamespaceFor(slug),
		ClusterID:       clusterID,
		DesiredState:    state,
		Resources:       models.Resources{Memory: req.Memory, CPU: req.CPU},
		Env:             plainEnv,
		SecretEnvEnc:    sealed,
		Storage:         storage,
		Ports:           tmpl.Ports,
		Expose:          req.Expose,
		Hibernation:     req.Hibernation,
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
	s.audit(r, srv.ID, "server.create", srv.Slug)
	writeJSON(w, http.StatusCreated, srv)
}

// resolveCluster maps a requested cluster slug to its ID, defaulting to the
// control plane's own (local) cluster when none is given.
func (s *Server) resolveCluster(slug string) (uint, error) {
	if strings.TrimSpace(slug) == "" {
		local, err := s.Store.EnsureLocalCluster()
		if err != nil {
			return 0, err
		}
		return local.ID, nil
	}
	c, err := s.Store.GetClusterBySlug(slug)
	if err != nil {
		return 0, fmt.Errorf("unknown cluster %q", slug)
	}
	return c.ID, nil
}

// resolveEnv merges template defaults with user-supplied values, enforcing the
// template's variable contract: only editable variables may be set, unknown keys
// are rejected (no arbitrary env injection into the container — e.g. LD_PRELOAD,
// JAVA_TOOL_OPTIONS), enum values must be valid, and required variables must end
// up non-empty.
func resolveEnv(tmpl *models.Template, reqEnv map[string]string) (map[string]string, error) {
	byEnv := make(map[string]models.TemplateVariable, len(tmpl.Variables))
	env := map[string]string{}
	for _, v := range tmpl.Variables {
		byEnv[v.EnvVariable] = v
		if v.Default != "" {
			env[v.EnvVariable] = v.Default
		}
	}
	for k, val := range reqEnv {
		v, ok := byEnv[k]
		if !ok {
			return nil, fmt.Errorf("unknown variable %q", k)
		}
		if !v.Editable {
			return nil, fmt.Errorf("variable %q is not editable", k)
		}
		if v.Type == models.VarEnum && len(v.Options) > 0 && !slices.Contains(v.Options, val) {
			return nil, fmt.Errorf("variable %q must be one of %v", k, v.Options)
		}
		env[k] = val
	}
	for _, v := range tmpl.Variables {
		if v.Required && strings.TrimSpace(env[v.EnvVariable]) == "" {
			return nil, fmt.Errorf("variable %q is required", v.EnvVariable)
		}
	}
	return env, nil
}

// checkQuota enforces a user's per-user quotas (admins are exempt). It sums the
// user's existing owned servers plus the new request against their limits.
func (s *Server) checkQuota(u *models.User, memory, cpu string) error {
	if u.IsAdmin || (u.MaxServers == 0 && u.MaxMemoryMB == 0 && u.MaxCPUMilli == 0) {
		return nil
	}
	// A memory/CPU quota only means something if every server it covers declares
	// a limit; otherwise an unlimited server counts as 0 and trivially bypasses
	// the quota while consuming unbounded resources. Require the matching limit.
	if u.MaxMemoryMB > 0 && strings.TrimSpace(memory) == "" {
		return errors.New("a memory limit is required (your account has a memory quota)")
	}
	if u.MaxCPUMilli > 0 && strings.TrimSpace(cpu) == "" {
		return errors.New("a CPU limit is required (your account has a CPU quota)")
	}
	owned, err := s.Store.ListServersByOwner(u.ID)
	if err != nil {
		return err
	}
	memMB, cpuM := int64(0), int64(0)
	for i := range owned {
		mb, c := resourceTotals(owned[i].Resources)
		memMB += mb
		cpuM += c
	}
	nmb, ncpu := resourceTotals(models.Resources{Memory: memory, CPU: cpu})
	if u.MaxServers > 0 && len(owned)+1 > u.MaxServers {
		return fmt.Errorf("quota exceeded: at most %d servers", u.MaxServers)
	}
	if u.MaxMemoryMB > 0 && memMB+nmb > u.MaxMemoryMB {
		return fmt.Errorf("quota exceeded: memory limit %d MB", u.MaxMemoryMB)
	}
	if u.MaxCPUMilli > 0 && cpuM+ncpu > u.MaxCPUMilli {
		return fmt.Errorf("quota exceeded: CPU limit %dm", u.MaxCPUMilli)
	}
	return nil
}

// resourceTotals converts a server's resource limits to (MB, millicores); zero
// when unset/unparseable.
func resourceTotals(rsc models.Resources) (mb, milli int64) {
	if rsc.Memory != "" {
		if q, err := resource.ParseQuantity(rsc.Memory); err == nil {
			mb = q.Value() / (1024 * 1024)
		}
	}
	if rsc.CPU != "" {
		if q, err := resource.ParseQuantity(rsc.CPU); err == nil {
			milli = q.MilliValue()
		}
	}
	return mb, milli
}

type updateServerRequest struct {
	// Expose, when present, reconfigures external reachability (and reallocates
	// or frees pool node ports accordingly).
	Expose *models.Expose `json:"expose"`
	// Hibernation, when present, updates the idle scale-to-zero policy.
	Hibernation *models.Hibernation `json:"hibernation"`
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermSettings)
	if !ok {
		return
	}
	var req updateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Hibernation != nil {
		if err := s.Store.UpdateServerHibernation(srv.ID, *req.Hibernation); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		srv.Hibernation = *req.Hibernation
		// Disabling auto-sleep on a currently-hibernated server must wake it:
		// otherwise no policy will ever scale it back up and it stays stuck at
		// zero replicas.
		if !req.Hibernation.Enabled && srv.Hibernated {
			_ = s.Store.Wake(srv.ID, time.Now())
			srv.Hibernated = false
		}
		s.audit(r, srv.ID, "server.hibernation", strconv.FormatBool(req.Hibernation.Enabled))
	}
	if req.Expose == nil {
		writeJSON(w, http.StatusOK, srv) // nothing else to change
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
	s.audit(r, srv.ID, "server.update", "expose="+string(expose.ServiceType()))
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
	srv, ok := s.requireServer(w, r, models.PermDelete)
	if !ok {
		return
	}
	// keepData decides the data lifecycle. The query param (sent by the UI's
	// delete dialog) wins; otherwise fall back to the server's stored policy.
	keep := srv.Storage.RetainOnDelete
	if q := r.URL.Query().Get("keepData"); q != "" {
		keep = q == "true"
	}
	cs, _, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
		return
	}
	// Retain the data volume first, while the PVC still exists to resolve its PV.
	if err := s.retainDataIfKept(r.Context(), cs, srv, keep); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Remove the DB rows BEFORE the namespace. Once the server row is gone the
	// reconciler won't recreate the workload, closing the window where a
	// concurrent reconcile could re-materialize the namespace between teardown
	// and row deletion. GCOrphanNamespaces is the backstop for the namespace.
	_ = s.Store.DeleteSchedulesForServer(srv.ID)
	_ = s.Store.DeleteBackupsForServer(srv.ID)
	_ = s.Store.DeleteAccessForServer(srv.ID)
	_ = s.Store.DeleteChannelsForServer(srv.ID)
	s.audit(r, 0, "server.delete", srv.Slug+" (keepData="+strconv.FormatBool(keep)+")")
	if err := s.Store.DeleteServer(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Best-effort immediate namespace teardown; if it fails, GCOrphanNamespaces
	// (which deletes managed namespaces with no matching server row) cleans up.
	if err := s.deleteNamespace(r.Context(), cs, srv.Namespace); err != nil {
		log.Printf("delete server %s: namespace teardown deferred to GC: %v", srv.Slug, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// retainDataIfKept switches a kept PVC's bound PersistentVolume to Retain so the
// underlying volume (and its data) survives the namespace/PVC deletion as a
// Released PV. hostPath data is inherently retained (deleting the namespace never
// touches the node path), so this is a no-op there.
func (s *Server) retainDataIfKept(ctx context.Context, cs kubernetes.Interface, srv *models.Server, keepData bool) error {
	if !keepData || srv.Storage.Type != models.StoragePVC {
		return nil
	}
	pv := boundPV(ctx, cs, srv.Namespace)
	if pv == "" {
		return nil
	}
	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`)
	if _, err := cs.CoreV1().PersistentVolumes().Patch(ctx, pv, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("retain volume %s: %w", pv, err)
	}
	// Surface the retained PV so the operator can recover/rebind it later.
	log.Printf("server %s deleted with keepData: retained PersistentVolume %q (now Released)", srv.Slug, pv)
	return nil
}

// deleteNamespace removes a server's namespace (cascading its objects), treating
// an already-absent namespace as success.
func (s *Server) deleteNamespace(ctx context.Context, cs kubernetes.Interface, ns string) error {
	err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// boundPV returns the PersistentVolume backing a server's data PVC, or "".
func boundPV(ctx context.Context, cs kubernetes.Interface, ns string) string {
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, reconciler.DataVolume, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvc.Spec.VolumeName
}

type powerRequest struct {
	Action string `json:"action"` // start | stop | restart | kill
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermPower)
	if !ok {
		return
	}
	// A suspended server is frozen by an admin; ordinary power actions are blocked
	// until an admin lifts the suspension.
	if srv.DesiredState == models.StateSuspended {
		writeError(w, http.StatusConflict, "server is suspended by an administrator")
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
		// Starting also wakes a hibernated server and resets its idle timer.
		_ = s.Store.Wake(srv.ID, time.Now())
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
	s.audit(r, srv.ID, "server.power", req.Action)
	writeJSON(w, http.StatusOK, map[string]string{"action": req.Action, "result": "accepted"})
}

type wakeRequest struct {
	Slug  string `json:"slug"`
	Token string `json:"token"`
}

// handleWake is the wake-on-connect callback from a server's activator: a valid
// per-server token wakes a hibernated, wake-on-connect server. It always answers
// 204 for any well-formed request — an unknown slug and a bad token are
// indistinguishable, so a pod on the cluster (e.g. an untrusted game container)
// can't probe which servers exist.
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	if ip := s.clientIP(r); !s.InternalLimiter.Allow(ip) {
		tooManyRequests(w, s.InternalLimiter.RetryAfter(ip))
		return
	}
	var req wakeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	srv, err := s.Store.GetServerBySlug(req.Slug)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	want := crypto.WakeToken(s.WakeKey, srv.Slug)
	if !hmac.Equal([]byte(req.Token), []byte(want)) {
		// Don't reveal that the slug exists; log for operators and no-op.
		log.Printf("wake: invalid token for %q", srv.Slug)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if srv.Hibernation.WakesOnConnect() && srv.Hibernated {
		if err := s.Store.Wake(srv.ID, time.Now()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.audit(r, srv.ID, "server.wake", "wake-on-connect")
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleActive is the proxy's activity heartbeat: a valid token bumps the
// server's idle timer (LastActiveAt). This is how UDP activity is measured —
// /proc/net/tcp can't see it. Always 204 (no existence leak).
func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	if ip := s.clientIP(r); !s.InternalLimiter.Allow(ip) {
		tooManyRequests(w, s.InternalLimiter.RetryAfter(ip))
		return
	}
	var req wakeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	srv, err := s.Store.GetServerBySlug(req.Slug)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if hmac.Equal([]byte(req.Token), []byte(crypto.WakeToken(s.WakeKey, srv.Slug))) {
		_ = s.Store.UpdateLastActive(srv.ID, time.Now())
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSuspend / handleUnsuspend are admin-only: a suspended server is scaled
// to zero by the reconciler and its owner cannot power it back on.
func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if err := s.Store.SetDesiredState(srv.ID, models.StateSuspended); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.suspend", "")
	writeJSON(w, http.StatusOK, map[string]string{"result": "suspended"})
}

func (s *Server) handleUnsuspend(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if srv.DesiredState != models.StateSuspended {
		writeError(w, http.StatusConflict, "server is not suspended")
		return
	}
	if err := s.Store.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.unsuspend", "")
	writeJSON(w, http.StatusOK, map[string]string{"result": "unsuspended"})
}

func (s *Server) deletePods(r *http.Request, srv *models.Server, grace *int64) error {
	cs, _, err := s.clientsFor(srv)
	if err != nil {
		return err
	}
	return cs.CoreV1().Pods(srv.Namespace).DeleteCollection(
		r.Context(),
		metav1.DeleteOptions{GracePeriodSeconds: grace},
		metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + srv.Slug},
	)
}

// ---- console ----

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermConsole)
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
	cs, cfg, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the response
	}
	defer conn.Close()
	_ = console.Stream(r.Context(), conn, cs, cfg, srv.Namespace, srv.Slug)
}

// ---- observability ----

func (s *Server) handleServerStats(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermView)
	if !ok {
		return
	}
	cs, _, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
		return
	}
	pod, err := console.FindRunningPod(r.Context(), cs, srv.Namespace, srv.Slug)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	u, err := stats.PodUsage(r.Context(), cs, srv.Namespace, pod)
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
