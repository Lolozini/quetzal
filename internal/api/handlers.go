package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

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

// slugSuffixLen is the length of the random suffix that makes a server slug
// unique independently of its (non-unique) display name.
const slugSuffixLen = 4

// serverSlugBase is the readable prefix of a server's slug, derived from the
// display name and capped to leave room for the "-<suffix>". Falls back to
// "server" when the name yields no slug characters (the name is a free label,
// so it may be all emoji/non-latin).
func serverSlugBase(name string) string {
	b := egg.Slugify(name)
	if max := maxServerSlugLen - slugSuffixLen - 1; len(b) > max {
		b = strings.TrimRight(b[:max], "-")
	}
	if b == "" {
		b = "server"
	}
	return b
}

// randSlugSuffix returns a short DNS-safe random string ([a-z0-9]).
func randSlugSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, slugSuffixLen)
	if _, err := rand.Read(b); err != nil {
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (i * 8))
		}
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

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
	// Email is optional, captured at first-run setup for password reset.
	Email string `json:"email"`
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
	email := strings.TrimSpace(req.Email)
	if email != "" && !looksLikeEmail(email) {
		writeError(w, http.StatusBadRequest, "invalid email")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	u := &models.User{Username: req.Username, PasswordHash: hash, Email: email, IsAdmin: true}
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
		_ = s.Store.DeleteSession(hashToken(token))
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
	// Store only the hash of the token (like API keys and reset tokens): a
	// read-only DB/backup leak then can't yield replayable live sessions. The
	// cookie carries the plaintext token; lookups hash it before matching.
	if err := s.Store.CreateSession(&models.Session{Token: hashToken(token), UserID: u.ID, ExpiresAt: exp}); err != nil {
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
	// Suggest ports from port-like variables for templates that declare none, so
	// the create form can pre-fill the per-server ports editor.
	for i := range ts {
		if len(ts[i].Ports) == 0 {
			ts[i].SuggestedPorts = models.DetectPorts(ts[i].Variables)
		}
	}
	writeJSON(w, http.StatusOK, ts)
}

// ---- servers ----

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var srvs []models.Server
	var err error
	if u.HasAdminPerm(models.AdminPermServers) {
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
	Name     string         `json:"name"`
	Template string         `json:"template"`
	Image    string         `json:"image"`
	Memory   string         `json:"memory"`
	CPU      string         `json:"cpu"`
	Storage  models.Storage `json:"storage"`
	// Ports, when non-empty, overrides the template's ports (e.g. for imported
	// eggs, which declare none — Pterodactyl allocates ports per server).
	Ports       []models.PortSpec  `json:"ports"`
	Env         map[string]string  `json:"env"`
	Expose      models.Expose      `json:"expose"`
	Hibernation models.Hibernation `json:"hibernation"`
	Cluster     string             `json:"cluster"` // target cluster slug ("" = local)
	Start       bool               `json:"start"`
	// EULAAccepted accepts the Minecraft EULA for templates with the "eula" egg
	// feature (ignored for templates that don't declare it).
	EULAAccepted bool `json:"eulaAccepted"`
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
	// The display name is a free, non-unique label (duplicates allowed, like
	// Pterodactyl). The server's stable identity is the slug — a readable prefix
	// derived from the name plus a short random suffix — generated once at create
	// and never changed, so renaming or reusing a name never touches resources.
	slugBase := serverSlugBase(req.Name)

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
	if storage.Type != models.StoragePVC {
		writeError(w, http.StatusBadRequest, "unsupported storage type (only pvc is supported)")
		return
	}
	if storage.Size == "" {
		storage.Size = "10Gi"
	}

	// Ports: imported eggs declare none (Pterodactyl allocates per server), so the
	// request may supply them; otherwise use the template's.
	ports := tmpl.Ports
	if len(req.Ports) > 0 {
		var perr error
		ports, perr = sanitizePorts(req.Ports)
		if perr != nil {
			writeError(w, http.StatusBadRequest, perr.Error())
			return
		}
	}

	if err := validateExpose(req.Expose, len(ports) > 0); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := validateResources(models.Resources{Memory: req.Memory, CPU: req.CPU}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the target cluster (default: the local / in-cluster cluster).
	clusterID, err := s.resolveCluster(req.Cluster)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// storageClass is admin-controlled per cluster, not chosen by tenants: take
	// the target cluster's default (empty = the cluster's own default class).
	if cl, err := s.Store.GetCluster(clusterID); err == nil {
		storage.StorageClass = cl.DefaultStorageClass
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
		DisplayName:       req.Name,
		OwnerID:           owner.ID,
		TemplateID:        tmpl.ID,
		TemplateVersion:   tmpl.Version,
		Image:             image,
		ClusterID:         clusterID,
		DesiredState:      state,
		Resources:         models.Resources{Memory: req.Memory, CPU: req.CPU},
		Env:               plainEnv,
		SecretEnvEnc:      sealed,
		InstallGeneration: 1,
		Storage:           storage,
		Ports:             ports,
		Expose:            req.Expose,
		Hibernation:       req.Hibernation,
		EULAAccepted:      req.EULAAccepted,
		Status:            models.Status{Phase: models.PhaseStopped},
	}
	// Assign a unique slug/namespace by inserting; the uniqueIndex on slug is the
	// source of truth, so on the (astronomically rare) random-suffix collision we
	// just regenerate and retry.
	var created bool
	for attempt := 0; attempt < 8; attempt++ {
		srv.ID = 0
		srv.Slug = slugBase + "-" + randSlugSuffix()
		srv.Namespace = reconciler.NamespaceFor(srv.Slug)
		err := s.Store.CreateServer(srv)
		if err == nil {
			created = true
			break
		}
		if errors.Is(err, store.ErrDuplicate) {
			continue
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !created {
		writeError(w, http.StatusInternalServerError, "could not allocate a unique server slug")
		return
	}

	// NodePort exposure draws stable ports from the control-plane pool, which
	// needs the server's ID, so allocate after the row exists.
	if req.Expose.ServiceType() == models.ExposeNodePort {
		allocated, err := s.allocateNodePorts(srv.ID, srv.Ports)
		if err != nil {
			_ = s.Store.DeleteServer(srv.ID) // avoid a half-configured record
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err := s.Store.UpdateServerNetworking(srv.ID, req.Expose, allocated); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		srv.Ports = allocated
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
			// Tolerate a client echoing a non-editable variable back at its
			// default (a no-op); only reject an actual attempt to change it.
			if val == v.Default {
				continue
			}
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
	if u.HasAdminPerm(models.AdminPermServers) || (u.MaxServers == 0 && u.MaxMemoryMB == 0 && u.MaxCPUMilli == 0) {
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

// httpErr carries a status + message out of the per-field update helpers.
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

// updateServerEnv re-resolves and persists the server's startup variables.
// Unspecified known variables keep their current value, and a blank secret
// variable is preserved (so saving the form never wipes a stored password).
func (s *Server) updateServerEnv(r *http.Request, srv *models.Server, reqEnv map[string]string) *httpErr {
	tmpl, err := s.Store.GetTemplate(srv.TemplateID)
	if err != nil {
		return &httpErr{http.StatusInternalServerError, "could not load template"}
	}
	currentSecret, err := s.Store.OpenSecrets(srv.SecretEnvEnc)
	if err != nil {
		return &httpErr{http.StatusInternalServerError, "could not read current secrets"}
	}
	current := map[string]string{}
	for k, v := range srv.Env {
		current[k] = v
	}
	for k, v := range currentSecret {
		current[k] = v
	}
	merged, verr := resolveEnvUpdate(tmpl, current, reqEnv)
	if verr != nil {
		return &httpErr{http.StatusBadRequest, verr.Error()}
	}
	secretNames := map[string]bool{}
	for _, v := range tmpl.Variables {
		if v.Secret {
			secretNames[v.EnvVariable] = true
		}
	}
	plainEnv, secretEnv := map[string]string{}, map[string]string{}
	for k, v := range merged {
		if secretNames[k] {
			secretEnv[k] = v
		} else {
			plainEnv[k] = v
		}
	}
	sealed, err := s.Store.SealSecrets(secretEnv)
	if err != nil {
		return &httpErr{http.StatusInternalServerError, "failed to seal secrets"}
	}
	if err := s.Store.UpdateServerEnv(srv.ID, plainEnv, sealed); err != nil {
		return &httpErr{http.StatusInternalServerError, err.Error()}
	}
	srv.Env = plainEnv
	srv.SecretEnvEnc = sealed
	s.audit(r, srv.ID, "server.env", "")
	return nil
}

// resolveEnvUpdate is resolveEnv's sibling for edits: it seeds from the server's
// current values (not just template defaults), keeps blank secrets, and enforces
// the same editable/enum/required contract.
func resolveEnvUpdate(tmpl *models.Template, current, reqEnv map[string]string) (map[string]string, error) {
	byEnv := make(map[string]models.TemplateVariable, len(tmpl.Variables))
	env := map[string]string{}
	for _, v := range tmpl.Variables {
		byEnv[v.EnvVariable] = v
		if v.Default != "" {
			env[v.EnvVariable] = v.Default
		}
	}
	for k, v := range current {
		if _, ok := byEnv[k]; ok {
			env[k] = v
		}
	}
	for k, val := range reqEnv {
		v, ok := byEnv[k]
		if !ok {
			return nil, fmt.Errorf("unknown variable %q", k)
		}
		if !v.Editable {
			// Tolerate a client echoing a non-editable variable back at its
			// default (a no-op); only reject an actual attempt to change it.
			if val == v.Default {
				continue
			}
			return nil, fmt.Errorf("variable %q is not editable", k)
		}
		if v.Secret && strings.TrimSpace(val) == "" {
			continue // keep the current secret
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

// updateServerResources validates and persists new CPU/memory limits, enforcing
// the owner's quota (admins bypass).
func (s *Server) updateServerResources(r *http.Request, srv *models.Server, rsc models.Resources) *httpErr {
	if err := validateResources(rsc); err != nil {
		return &httpErr{http.StatusBadRequest, err.Error()}
	}
	if editor := userFrom(r.Context()); !editor.HasAdminPerm(models.AdminPermServers) {
		if owner, err := s.Store.GetUser(srv.OwnerID); err == nil {
			if err := s.checkResourceQuotaForUpdate(owner, srv.ID, rsc.Memory, rsc.CPU); err != nil {
				return &httpErr{http.StatusForbidden, err.Error()}
			}
		}
	}
	if err := s.Store.UpdateServerResources(srv.ID, rsc); err != nil {
		return &httpErr{http.StatusInternalServerError, err.Error()}
	}
	srv.Resources = rsc
	s.audit(r, srv.ID, "server.resources", strings.TrimSpace(rsc.Memory+" "+rsc.CPU))
	return nil
}

// minMemoryBytes is the smallest memory limit we accept. A bare number is bytes
// in Kubernetes quantity syntax, so "4" means 4 bytes — almost always a
// forgotten unit. Anything under a few MiB can't run a container and makes the
// kubelet fail the pod sandbox with a cryptic systemd/cgroup error, so we reject
// it up front with a hint instead.
const minMemoryBytes = 4 * 1024 * 1024 // 4Mi

func validateResources(rsc models.Resources) error {
	if strings.TrimSpace(rsc.Memory) != "" {
		q, err := resource.ParseQuantity(rsc.Memory)
		if err != nil {
			return fmt.Errorf("invalid memory %q", rsc.Memory)
		}
		if q.CmpInt64(minMemoryBytes) < 0 {
			return fmt.Errorf("memory limit %q is too small — did you forget a unit? e.g. 4Gi or 512Mi", rsc.Memory)
		}
	}
	if strings.TrimSpace(rsc.CPU) != "" {
		q, err := resource.ParseQuantity(rsc.CPU)
		if err != nil {
			return fmt.Errorf("invalid cpu %q", rsc.CPU)
		}
		if q.Sign() < 0 {
			return fmt.Errorf("cpu limit %q cannot be negative", rsc.CPU)
		}
	}
	return nil
}

// checkResourceQuotaForUpdate re-checks the owner's memory/CPU quota for an
// edited server, counting the owner's other servers plus the new request (the
// edited server's old allocation is excluded since it's being replaced). Server
// count is unaffected by an edit, so it isn't checked here.
func (s *Server) checkResourceQuotaForUpdate(u *models.User, serverID uint, memory, cpu string) error {
	if u.HasAdminPerm(models.AdminPermServers) || (u.MaxMemoryMB == 0 && u.MaxCPUMilli == 0) {
		return nil
	}
	if u.MaxMemoryMB > 0 && strings.TrimSpace(memory) == "" {
		return errors.New("a memory limit is required (the owner has a memory quota)")
	}
	if u.MaxCPUMilli > 0 && strings.TrimSpace(cpu) == "" {
		return errors.New("a CPU limit is required (the owner has a CPU quota)")
	}
	owned, err := s.Store.ListServersByOwner(u.ID)
	if err != nil {
		return err
	}
	memMB, cpuM := int64(0), int64(0)
	for i := range owned {
		if owned[i].ID == serverID {
			continue
		}
		mb, c := resourceTotals(owned[i].Resources)
		memMB += mb
		cpuM += c
	}
	nmb, ncpu := resourceTotals(models.Resources{Memory: memory, CPU: cpu})
	if u.MaxMemoryMB > 0 && memMB+nmb > u.MaxMemoryMB {
		return fmt.Errorf("quota exceeded: memory limit %d MB", u.MaxMemoryMB)
	}
	if u.MaxCPUMilli > 0 && cpuM+ncpu > u.MaxCPUMilli {
		return fmt.Errorf("quota exceeded: CPU limit %dm", u.MaxCPUMilli)
	}
	return nil
}

type updateServerRequest struct {
	// Expose, when present, reconfigures external reachability (and reallocates
	// or frees pool node ports accordingly).
	Expose *models.Expose `json:"expose"`
	// Hibernation, when present, updates the idle scale-to-zero policy.
	Hibernation *models.Hibernation `json:"hibernation"`
	// SFTP, when present, toggles the SFTP sidecar.
	SFTP *models.SFTPConfig `json:"sftp"`
	// EULAAccepted, when present, sets Minecraft EULA acceptance (templates with
	// the "eula" egg feature). Applied on the next reconcile.
	EULAAccepted *bool `json:"eulaAccepted"`
	// Env, when present, edits the server's startup variables (validated against
	// the template's variable contract). Secret variables left blank are kept.
	Env *map[string]string `json:"env"`
	// Resources, when present, updates the CPU/memory limits (re-checked against
	// the owner's quota). Applied on the next reconcile, which restarts the pod.
	Resources *models.Resources `json:"resources"`
	// Ports, when present, replaces the server's per-server port set (the ports
	// editor for imported eggs). Reallocates pool node ports as needed and rolls
	// the pod on the next reconcile.
	Ports *[]models.PortSpec `json:"ports"`
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermSettings)
	if !ok {
		return
	}
	if transferInProgress(w, srv) {
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
	if req.SFTP != nil {
		if err := s.Store.UpdateServerSFTP(srv.ID, *req.SFTP); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		srv.SFTP = *req.SFTP
		s.audit(r, srv.ID, "server.sftp", strconv.FormatBool(req.SFTP.Enabled))
	}
	if req.EULAAccepted != nil {
		if err := s.Store.UpdateServerEULA(srv.ID, *req.EULAAccepted); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		srv.EULAAccepted = *req.EULAAccepted
		s.audit(r, srv.ID, "server.eula", strconv.FormatBool(*req.EULAAccepted))
	}
	if req.Env != nil {
		if err := s.updateServerEnv(r, srv, *req.Env); err != nil {
			writeError(w, err.code, err.msg)
			return
		}
	}
	if req.Resources != nil {
		if err := s.updateServerResources(r, srv, *req.Resources); err != nil {
			writeError(w, err.code, err.msg)
			return
		}
	}
	if req.Ports == nil && req.Expose == nil {
		writeJSON(w, http.StatusOK, srv) // nothing else to change
		return
	}

	// Networking: the port set and/or the exposure. Recompute them together so
	// the pool node-port allocations stay consistent with both.
	portsChanged := req.Ports != nil
	newPorts := srv.Ports
	if portsChanged {
		sanitized, perr := sanitizePorts(*req.Ports)
		if perr != nil {
			writeError(w, http.StatusBadRequest, perr.Error())
			return
		}
		newPorts = sanitized
	}
	expose := srv.Expose
	if req.Expose != nil {
		expose = *req.Expose
	}
	if err := validateExpose(expose, len(newPorts) > 0); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var ports []models.PortSpec
	if expose.ServiceType() == models.ExposeNodePort {
		// Free node ports for game ports no longer present so the pool set matches
		// the new list. Ports that survive keep their allocation (allocateNodePorts
		// reuses by name), so unchanged external ports stay stable. The SFTP port
		// has its own allocation key and is left untouched.
		if portsChanged {
			keep := map[string]bool{}
			for _, p := range newPorts {
				keep[portAllocName(p)] = true
			}
			for _, p := range srv.Ports {
				if !keep[portAllocName(p)] {
					_ = s.Store.ReleaseNodePort(srv.ID, portAllocName(p))
				}
			}
		}
		allocated, err := s.allocateNodePorts(srv.ID, newPorts)
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		ports = allocated
	} else {
		// Not NodePort: release every game port's pool allocation (leaving SFTP).
		for _, p := range srv.Ports {
			_ = s.Store.ReleaseNodePort(srv.ID, portAllocName(p))
		}
		ports = clearNodePorts(newPorts)
	}
	if err := s.Store.UpdateServerNetworking(srv.ID, expose, ports); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	srv.Expose = expose
	srv.Ports = ports
	s.audit(r, srv.ID, "server.update", fmt.Sprintf("expose=%s ports=%d", expose.ServiceType(), len(ports)))
	writeJSON(w, http.StatusOK, srv)
}

// handleReinstallServer re-runs a server's install script (egg
// scripts.installation), optionally wiping the data volume first. It bumps the
// install generation so the install init container re-runs; the change rolls the
// pod on the next reconcile (and, for a stopped server, runs on next start).
func (s *Server) handleReinstallServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermSettings)
	if !ok {
		return
	}
	if transferInProgress(w, srv) {
		return
	}
	tmpl, err := s.Store.GetTemplate(srv.TemplateID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load template")
		return
	}
	if tmpl.Install == nil || strings.TrimSpace(tmpl.Install.Script) == "" {
		writeError(w, http.StatusBadRequest, "this server's template has no install script to run")
		return
	}
	var req struct {
		WipeData bool `json:"wipeData"`
	}
	_ = decodeJSON(r, &req) // body optional; default is keep-data
	if err := s.Store.BumpInstallGeneration(srv.ID, req.WipeData); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, srv.ID, "server.reinstall", fmt.Sprintf("wipeData=%v", req.WipeData))
	writeJSON(w, http.StatusOK, map[string]any{"status": "reinstalling", "wipeData": req.WipeData})
}

// allocateNodePorts reserves a stable pool node port for each of a server's
// ports, returning the port list with NodePort fields set.
func (s *Server) allocateNodePorts(serverID uint, ports []models.PortSpec) ([]models.PortSpec, error) {
	out := make([]models.PortSpec, len(ports))
	copy(out, ports)
	for i := range out {
		np, err := s.Store.AllocateNodePort(serverID, portAllocName(out[i]), s.NodePortMin, s.NodePortMax)
		if err != nil {
			return nil, err
		}
		out[i].NodePort = np
	}
	return out, nil
}

// portAllocName is a port's key in the node-port pool: its explicit name, or a
// stable name derived from the port number when unnamed. Must match between
// allocation and release so a port frees the same key it reserved.
func portAllocName(p models.PortSpec) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("p%d", p.Port)
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

// sanitizePorts validates and normalizes user-supplied per-server ports (used by
// servers created from eggs, which declare none): valid port range, TCP/UDP
// protocol (default TCP), a generated name when blank, and exactly one primary
// (the first when none is marked). The NodePort field is ignored — it is
// allocated server-side from the pool.
func sanitizePorts(in []models.PortSpec) ([]models.PortSpec, error) {
	out := make([]models.PortSpec, 0, len(in))
	primaries := 0
	seen := map[string]bool{}
	for _, p := range in {
		if p.Port < 1 || p.Port > 65535 {
			return nil, fmt.Errorf("invalid port %d (want 1-65535)", p.Port)
		}
		proto := strings.ToUpper(strings.TrimSpace(p.Protocol))
		if proto == "" {
			proto = "TCP"
		}
		if proto != "TCP" && proto != "UDP" {
			return nil, fmt.Errorf("invalid protocol %q (want TCP or UDP)", p.Protocol)
		}
		key := fmt.Sprintf("%d/%s", p.Port, proto)
		if seen[key] {
			return nil, fmt.Errorf("duplicate port %s", key)
		}
		seen[key] = true
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = fmt.Sprintf("p%d", p.Port)
		}
		if p.Primary {
			primaries++
		}
		out = append(out, models.PortSpec{Name: name, Port: p.Port, Protocol: proto, Primary: p.Primary})
	}
	if primaries > 1 {
		return nil, errors.New("only one port can be primary")
	}
	if primaries == 0 && len(out) > 0 {
		out[0].Primary = true
	}
	return out, nil
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.requireServer(w, r, models.PermDelete)
	if !ok {
		return
	}
	cs, _, err := s.clientsFor(srv)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "target cluster unavailable: "+err.Error())
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
	s.dropServerDatabases(r.Context(), srv.ID)
	s.audit(r, 0, "server.delete", srv.Slug)
	if err := s.Store.DeleteServer(srv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Delete the namespace, cascading its pod, service, config AND the data PVC —
	// so the volume and its data are reclaimed and no orphaned resources are left.
	// Best-effort; if it fails, GCOrphanNamespaces (which deletes managed
	// namespaces with no matching server row) cleans up.
	if err := s.deleteNamespace(r.Context(), cs, srv.Namespace); err != nil {
		log.Printf("delete server %s: namespace teardown deferred to GC: %v", srv.Slug, err)
	}
	w.WriteHeader(http.StatusNoContent)
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
	if transferInProgress(w, srv) {
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
	if !s.requireAdminPerm(w, r, models.AdminPermServers) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if transferInProgress(w, srv) {
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
	if !s.requireAdminPerm(w, r, models.AdminPermServers) {
		return
	}
	srv, ok := s.lookupServer(w, r)
	if !ok {
		return
	}
	if transferInProgress(w, srv) {
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
	cs, cfg, err := s.clientsFor(srv)
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
	resp := map[string]any{
		"cpuMillicores": u.CPUMillicores,
		"memoryBytes":   u.MemoryBytes,
		"cpuLimit":      srv.Resources.CPU,
		"memoryLimit":   srv.Resources.Memory,
	}
	// Network + disk aren't in metrics-server, so read them from the pod in one
	// exec: cumulative net counters (the client derives a rate) and df of the
	// data volume. Best-effort — a distroless image without a shell just omits
	// these fields rather than failing the whole stats call.
	s.addIOStats(r.Context(), cs, cfg, srv, pod, resp)
	writeJSON(w, http.StatusOK, resp)
}

// addIOStats augments resp with rxBytes/txBytes (cumulative) and data-volume
// disk usage, read from the running pod. Errors are swallowed by design.
//
// Disk is reported as actual usage of the data volume (du) against the PVC's
// declared size — NOT df: on local-path (hostPath-backed) PVCs df reports the
// whole host filesystem, not the per-server volume. du is walked at most once
// per diskUsageTTL (cached), so the 4s stats poll stays cheap; net counters are
// still read every poll.
func (s *Server) addIOStats(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, srv *models.Server, pod string, resp map[string]any) {
	ioCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	root := s.dataRoot(srv)

	used, fresh := s.cachedDiskUsage(srv.ID)
	script := `cat /proc/net/dev 2>/dev/null`
	if !fresh {
		script += `; echo "@@DU@@"; du -sk "$1" 2>/dev/null`
	}
	var out strings.Builder
	if err := console.Exec(ioCtx, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", script, "_", root}, nil, &out); err != nil {
		return
	}
	netPart, duPart, hasDU := strings.Cut(out.String(), "@@DU@@")
	rx, tx := stats.ParseNetDev([]byte(netPart))
	resp["rxBytes"] = rx
	resp["txBytes"] = tx

	if !fresh && hasDU {
		if u := stats.ParseDuUsed([]byte(duPart)); u >= 0 {
			used, fresh = u, true
			s.setDiskUsage(srv.ID, u)
		}
	}
	if fresh {
		resp["diskUsedBytes"] = used
		// Total = the PVC's declared size (there is no per-volume quota to read on
		// local-path). Omitted when unparseable, so the UI just hides the bar.
		if q, err := resource.ParseQuantity(srv.Storage.Size); err == nil && q.Value() > 0 {
			resp["diskTotalBytes"] = q.Value()
		}
	}
}

// diskUsageTTL bounds how often du walks a server's data volume for the stats
// endpoint (usage changes slowly; the poll runs every few seconds).
const diskUsageTTL = 30 * time.Second

// cachedDiskUsage returns a server's last du reading if still within the TTL.
func (s *Server) cachedDiskUsage(id uint) (int64, bool) {
	s.diskUsageMu.Lock()
	defer s.diskUsageMu.Unlock()
	if smp, ok := s.diskUsage[id]; ok && time.Since(smp.at) < diskUsageTTL {
		return smp.usedBytes, true
	}
	return 0, false
}

// setDiskUsage records a fresh du reading for a server.
func (s *Server) setDiskUsage(id uint, used int64) {
	s.diskUsageMu.Lock()
	defer s.diskUsageMu.Unlock()
	if s.diskUsage == nil {
		s.diskUsage = map[uint]diskSample{}
	}
	s.diskUsage[id] = diskSample{usedBytes: used, at: time.Now()}
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
