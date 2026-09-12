// Package reconciler projects servers (the DB source of truth) into native
// Kubernetes objects, and writes observed status back to the DB.
package reconciler

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// Reconciler turns desired DB state into Kubernetes objects.
type Reconciler struct {
	Client client.Client
	Store  *store.Store

	// NamespacedRole is the ClusterRole the control plane binds to itself in each
	// namespace it creates, instead of holding that access cluster-wide. Empty
	// disables the binding, which is what an install predating the split, or a
	// cluster registered with an administrator's kubeconfig, looks like.
	NamespacedRole string

	// warned de-duplicates warnings that would otherwise repeat every resync.
	warnMu sync.Mutex
	warned map[string]bool

	// identity* cache who this control plane authenticates as on this cluster.
	identityMu      sync.Mutex
	identityDone    bool
	identityOK      bool
	identitySubject rbacv1.Subject

	// OnStop, if set, is called just before a running server is scaled to zero
	// so a graceful stop command can be delivered to the container (via the
	// console attach path). It is best-effort. Injected by the controller to
	// avoid an import cycle with the console package.
	OnStop func(ctx context.Context, namespace, slug, stopCommand string) error

	// Wake-on-connect: when ActivatorImage is set, a server with wake-on-connect
	// (drop) or proxy mode gets an activator. WakeURL/ActiveURL are the control
	// plane callbacks; WakeKey signs the per-server callback token.
	ActivatorImage string
	WakeURL        string
	ActiveURL      string
	WakeKey        []byte

	// ExtraEgressCIDRs are private ranges every server may reach, on top of the
	// public internet. The default policy denies private address space so the
	// cluster network is out of reach; an operator whose servers need something
	// there — an external database named by DNS, a license server on the LAN —
	// names its range here. Injected by the controller from QUETZAL_EGRESS_ALLOW.
	ExtraEgressCIDRs []string

	// ControlPlaneNamespace is where the apiserver and controller run. A managed
	// database's ingress policy has to let it in: it is the one that creates and
	// drops databases over 3306. Empty means unknown, and the policy is then not
	// written at all rather than written in a way that would lock provisioning
	// out — a database nobody can provision is a worse outcome than one reachable
	// from inside the cluster, which is what it was before.
	ControlPlaneNamespace string

	// NodePortMin/NodePortMax bound the node-port pool the SFTP Service draws
	// from (0 = the store's defaults). Same pool as the game ports, so SFTP and
	// game allocations never collide. Injected by the controller.
	NodePortMin int32
	NodePortMax int32

	// ClusterID is the cluster this reconciler drives. It selects that cluster's
	// endpoint hostname, since each cluster has its own nodes and a single
	// panel-wide name would advertise one cluster's address for another's
	// servers. 0 = the control plane's own cluster.
	ClusterID uint

	// InstanceID identifies this control plane (see InstanceLabel). Namespaces
	// are stamped with it, and orphan collection only reclaims namespaces that
	// are unowned or owned by this instance — two control planes sharing a
	// cluster would otherwise delete each other's servers. Collection is a no-op
	// while this is empty.
	InstanceID string
}

// New returns a Reconciler. It resolves the control plane's instance id from the
// store so ownership stamping and orphan collection are correct by construction;
// if that read fails, InstanceID stays empty and collection is skipped rather
// than risking another instance's namespaces.
func New(c client.Client, s *store.Store) *Reconciler {
	r := &Reconciler{Client: c, Store: s}
	if id, err := s.InstanceID(); err == nil {
		r.InstanceID = id
	}
	return r
}

// ReconcileServer ensures the cluster matches the DB for one server, then
// updates its status in the DB.
func (r *Reconciler) ReconcileServer(ctx context.Context, id uint) error {
	srv, err := r.Store.GetServer(id)
	if err != nil {
		return err
	}
	tmpl, err := r.Store.GetTemplate(srv.TemplateID)
	if err != nil {
		return fmt.Errorf("server %s: template: %w", srv.Slug, err)
	}
	// A server's namespace is derived from its slug at creation and never changes
	// afterwards, so the two can only disagree if the row was tampered with —
	// which would aim everything below, up to and including namespace deletion,
	// at whatever the row said. Refuse rather than derive: if this ever fires it
	// is either an attack or a schema change, and both want a human.
	if want := NamespaceFor(srv.Slug); srv.Namespace != want {
		return fmt.Errorf("server %s: namespace %q does not match its slug (expected %q); refusing to act on it",
			srv.Slug, srv.Namespace, want)
	}

	if err := r.ensureNamespace(ctx, srv); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	// Before anything else in there: every step below needs the access this
	// grants, and on an upgrade the namespace already exists without it.
	r.ensureRoleBinding(ctx, srv.Namespace)
	if err := r.ensureResourceQuota(ctx, srv); err != nil {
		return fmt.Errorf("resourcequota: %w", err)
	}
	// SFTP supporting objects (host key, authorized_keys, Service) must exist
	// before the Deployment references them.
	if err := r.ensureSFTP(ctx, srv); err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	if pvc := BuildPVC(srv); pvc != nil {
		if err := r.ensurePVC(ctx, pvc); err != nil {
			return fmt.Errorf("pvc: %w", err)
		}
	}

	// Materialize sensitive env into a per-server Secret (referenced by the
	// Deployment via secretKeyRef). Values are decrypted from the DB here.
	secretEnv, err := r.Store.OpenSecrets(srv.SecretEnvEnc)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if sec := BuildSecret(srv, secretEnv); sec != nil {
		if err := r.ensureSecret(ctx, sec); err != nil {
			return fmt.Errorf("secret: %w", err)
		}
	}
	secretKeys := make([]string, 0, len(secretEnv))
	for k := range secretEnv {
		secretKeys = append(secretKeys, k)
	}
	// Graceful stop: when transitioning a currently-running server to a
	// non-running state and the template defines a stop command, deliver it
	// before scaling to zero (SIGTERM + termination grace period follow).
	if srv.DesiredState != models.StateRunning && isConsoleStop(tmpl.StopCommand) && r.OnStop != nil {
		if running, _ := r.deploymentRunning(ctx, srv.Namespace); running {
			if err := r.OnStop(ctx, srv.Namespace, srv.Slug, tmpl.StopCommand); err != nil {
				log.Printf("graceful stop for %s (continuing to scale down): %v", srv.Slug, err)
			}
		}
	}

	// The data-manager pod (files + SFTP) mounts the data volume permanently; the
	// game pod is co-located with it via podAffinity. Ensure it before the game
	// Deployment so the game pod has a node to anchor to.
	if err := r.ensureDataDeployment(ctx, srv, tmpl); err != nil {
		return fmt.Errorf("data manager: %w", err)
	}

	if err := r.ensureDeployment(ctx, srv, tmpl, secretKeys); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	// Wake-on-connect: an activator may front the server. In proxy mode it is
	// always in path (and needs an internal backend Service); in drop mode it
	// only appears while hibernated. The public Service selector points at the
	// activator when one is fronting, else at the real workload.
	proxy := r.proxyActive(srv, tmpl)
	drop := r.dropActive(srv, tmpl)
	if err := r.ensureInternalService(ctx, srv, tmpl, proxy); err != nil {
		return fmt.Errorf("internal service: %w", err)
	}
	if err := r.ensureActivator(ctx, srv, tmpl, proxy, drop); err != nil {
		return fmt.Errorf("activator: %w", err)
	}
	// A Service requires at least one port; skip it for portless servers.
	if len(serverPorts(srv, tmpl)) > 0 {
		if err := r.ensureService(ctx, srv, tmpl, proxy || drop); err != nil {
			return fmt.Errorf("service: %w", err)
		}
	}
	if err := r.ensureNetworkPolicy(ctx, srv, tmpl); err != nil {
		return fmt.Errorf("networkpolicy: %w", err)
	}

	return r.updateStatus(ctx, srv, tmpl)
}

// DeleteServer tears down a server by deleting its namespace (cascades).
func (r *Reconciler) DeleteServer(ctx context.Context, srv *models.Server) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: srv.Namespace}}
	if err := r.Client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// GCOrphanNamespaces deletes Quetzal-managed namespaces whose server slug is no
// longer present in the DB (i.e. the server row was removed). This provides
// teardown for deleted servers in the Phase 0 resync model.
//
// "Orphan" is relative to *this* control plane's database, so a namespace owned
// by another instance sharing the cluster is never touched — it looks orphaned
// here while being perfectly alive there. Ownership is read from InstanceLabel;
// an unlabelled namespace predates the label and is adopted, which keeps
// cleanup working for the ordinary single-instance install. Collection is
// skipped entirely when this reconciler has no instance id, since nothing could
// then be distinguished from another instance's namespaces.
func (r *Reconciler) GCOrphanNamespaces(ctx context.Context, validSlugs map[string]bool) error {
	if r.InstanceID == "" {
		return nil
	}
	var list corev1.NamespaceList
	if err := r.Client.List(ctx, &list, client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		return err
	}
	for i := range list.Items {
		ns := &list.Items[i]
		slug := ns.Labels[serverLabel]
		if slug == "" || validSlugs[slug] {
			continue
		}
		if owner := ns.Labels[InstanceLabel]; owner != "" && owner != r.InstanceID {
			continue // belongs to another control plane
		}
		// The labels are Quetzal's own, but a label is not proof it created the
		// namespace — it writes them wherever a server row points. Deleting on
		// labels alone would follow a tampered row into someone else's namespace.
		if ns.Name != NamespaceFor(slug) {
			log.Printf("refusing to collect namespace %q: labelled for server %q, which belongs in %q", ns.Name, slug, NamespaceFor(slug))
			continue
		}
		if ns.DeletionTimestamp != nil {
			continue // already terminating
		}
		if err := r.Client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureNamespace(ctx context.Context, s *models.Server) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		ns.Labels = mergeLabels(ns.Labels, labelsFor(s))
		// Stamp ownership so orphan collection can tell this control plane's
		// namespaces from another's (see InstanceLabel). Also adopts namespaces
		// created before the label existed.
		if r.InstanceID != "" {
			ns.Labels[InstanceLabel] = r.InstanceID
		}
		return nil
	})
	if err == nil || !apierrors.IsForbidden(err) {
		return err
	}
	// Having the namespace is a hard requirement; re-labelling one that already
	// exists is not. This is the first step of a server's reconcile, so letting a
	// label write fail the whole call freezes everything after it — the
	// Deployment, the Service, the SFTP keys — while the panel still reports the
	// server as running. A role without update on namespaces is enough to trigger
	// it: adding any new label (as the ownership stamp did) turns a create-only
	// reconcile into an update on every pre-existing namespace.
	if getErr := r.Client.Get(ctx, client.ObjectKey{Name: s.Namespace}, &corev1.Namespace{}); getErr != nil {
		return err
	}
	r.warnOnce("namespace-labels", "cannot update namespace labels (%v); servers keep reconciling, but orphan-collection ownership will not be stamped on existing namespaces", err)
	return nil
}

// warnOnce logs a message the first time a given key is seen, so a condition
// that repeats on every 15s resync is reported without flooding the log.
func (r *Reconciler) warnOnce(key, format string, args ...any) {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	log.Printf(format, args...)
}

func (r *Reconciler) ensureResourceQuota(ctx context.Context, s *models.Server) error {
	return r.apply(ctx, BuildResourceQuota(s))
}

// ensureSFTP reconciles the SFTP sidecar's supporting objects: a stable host key
// (generated once), the authorized_keys ConfigMap (kept in sync with the users
// who hold file access), and the NodePort Service. When SFTP is disabled the
// Service and ConfigMap are removed (the host key is kept so re-enabling doesn't
// change it). Requires a system image (the SFTP binary lives there).
func (r *Reconciler) ensureSFTP(ctx context.Context, s *models.Server) error {
	if !s.SFTP.Enabled || r.ActivatorImage == "" {
		if !s.SFTP.Enabled {
			r.deleteSFTP(ctx, s)
		}
		return nil
	}
	if err := r.ensureSFTPHostKey(ctx, s); err != nil {
		return fmt.Errorf("sftp host key: %w", err)
	}
	keys, err := r.Store.ListAuthorizedSSHKeys(s.ID)
	if err != nil {
		return fmt.Errorf("sftp authorized keys: %w", err)
	}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k.PublicKey)
	}
	if err := r.apply(ctx, BuildSFTPAuthKeysConfigMap(s, lines)); err != nil {
		return fmt.Errorf("sftp configmap: %w", err)
	}
	// Draw the SFTP NodePort from the same pool as the game ports (stable per
	// server, no collision with Kubernetes' own auto-assignment).
	nodePort, err := r.Store.AllocateNodePort(s.ID, SFTPPortName, r.NodePortMin, r.NodePortMax)
	if err != nil {
		return fmt.Errorf("sftp node port: %w", err)
	}
	if err := r.apply(ctx, BuildSFTPService(s, nodePort)); err != nil {
		return fmt.Errorf("sftp service: %w", err)
	}
	return nil
}

// ensureSFTPHostKey creates a stable SSH host key Secret if absent.
func (r *Reconciler) ensureSFTPHostKey(ctx context.Context, s *models.Server) error {
	key := client.ObjectKey{Namespace: s.Namespace, Name: SFTPHostKeySecret}
	if err := r.Client.Get(ctx, key, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SFTPHostKeySecret, Namespace: s.Namespace, Labels: labelsFor(s)},
		Data:       map[string][]byte{SFTPHostKeyField: hostKey},
	}
	if err := r.Client.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *Reconciler) deleteSFTP(ctx context.Context, s *models.Server) {
	_ = r.Client.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: SFTPServiceName, Namespace: s.Namespace}})
	_ = r.Client.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SFTPAuthKeysConfigMap, Namespace: s.Namespace}})
	// Return the SFTP node port to the pool so it can be reused.
	_ = r.Store.ReleaseNodePort(s.ID, SFTPPortName)
}

// ensureDataDeployment reconciles the always-on data-manager Deployment (files +
// SFTP). It normally runs one replica, but scales to zero while a restore is
// active for the server: a restore overwrites the data volume in place and needs
// exclusive write access, which it can't get while the data-manager holds the
// ReadWriteOnce mount. Once the restore finishes, the next reconcile brings it
// back.
func (r *Reconciler) ensureDataDeployment(ctx context.Context, s *models.Server, t *models.Template) error {
	replicas := int32(1)
	if active, err := r.Store.HasActiveRestore(s.ID); err != nil {
		return fmt.Errorf("check active restore: %w", err)
	} else if active {
		replicas = 0
	}
	return r.apply(ctx, BuildDataDeployment(s, t, r.ActivatorImage, replicas))
}

func (r *Reconciler) ensurePVC(ctx context.Context, want *corev1.PersistentVolumeClaim) error {
	// PVC spec is largely immutable: create if absent, otherwise leave as-is.
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, want)
	}
	return err
}

// fieldOwner identifies Quetzal in server-side-apply managed fields.
const fieldOwner = "quetzal-controller"

// apply performs a server-side apply. Unlike overwriting the whole spec on each
// reconcile, SSA is idempotent and leaves server-defaulted fields untouched, so
// unchanged objects produce no write churn.
func (r *Reconciler) apply(ctx context.Context, obj client.Object) error {
	return r.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

func (r *Reconciler) ensureDeployment(ctx context.Context, s *models.Server, t *models.Template, secretKeys []string) error {
	return r.apply(ctx, BuildDeployment(s, t, r.ActivatorImage, secretKeys))
}

func (r *Reconciler) ensureService(ctx context.Context, s *models.Server, t *models.Template, activator bool) error {
	return r.apply(ctx, BuildService(s, t, activator))
}

// proxyActive reports whether the always-in-path proxy should front this server
// (hibernation + proxy mode + at least one port + an image to run).
func (r *Reconciler) proxyActive(s *models.Server, t *models.Template) bool {
	// Require a callback URL too: a proxy with no way to wake/heartbeat would let
	// the server hibernate with players and never wake.
	if r.ActivatorImage == "" || r.WakeURL == "" || !s.Hibernation.Enabled || !s.Hibernation.Proxy {
		return false
	}
	return len(serverPorts(s, t)) > 0
}

// dropActive reports whether the lightweight wake-and-drop activator should
// front this server (hibernated + wake-on-connect, not proxy, a TCP port).
func (r *Reconciler) dropActive(s *models.Server, t *models.Template) bool {
	if r.ActivatorImage == "" || r.WakeURL == "" || s.Hibernation.Proxy || !s.Hibernated || !s.Hibernation.WakeOnConnect {
		return false
	}
	return hasTCPPort(serverPorts(s, t))
}

// ensureActivator creates the activator Deployment for the active mode, or
// removes it when neither mode applies.
func (r *Reconciler) ensureActivator(ctx context.Context, s *models.Server, t *models.Template, proxy, drop bool) error {
	if !proxy && !drop {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ActivatorName, Namespace: s.Namespace}}
		if err := r.Client.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	return r.apply(ctx, BuildActivatorDeployment(s, t, ActivatorParams{
		Image:     r.ActivatorImage,
		WakeURL:   r.WakeURL,
		ActiveURL: r.ActiveURL,
		Token:     crypto.WakeToken(r.WakeKey, s.Slug),
		Proxy:     proxy,
	}))
}

// ensureInternalService maintains the proxy's stable backend Service.
func (r *Reconciler) ensureInternalService(ctx context.Context, s *models.Server, t *models.Template, proxy bool) error {
	if !proxy || len(serverPorts(s, t)) == 0 {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: InternalServiceName, Namespace: s.Namespace}}
		if err := r.Client.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	return r.apply(ctx, BuildInternalService(s, t))
}

func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, s *models.Server, t *models.Template) error {
	return r.apply(ctx, BuildNetworkPolicy(s, t, r.egressPeersFor(s)))
}

// egressPeersFor lists what this server may reach inside the private address
// space the default policy denies: the managed databases it has been given, plus
// whatever the operator allowed cluster-wide.
//
// An external database named by DNS cannot be expressed here — a NetworkPolicy
// has no notion of hostnames — so one on a private address needs its range in
// ExtraEgressCIDRs. A public one is already covered by the internet rule.
func (r *Reconciler) egressPeersFor(s *models.Server) []EgressPeer {
	peers := make([]EgressPeer, 0, len(r.ExtraEgressCIDRs))
	for _, c := range r.ExtraEgressCIDRs {
		peers = append(peers, EgressPeer{CIDR: c})
	}
	if r.Store == nil {
		return peers
	}
	dbs, err := r.Store.ListServerDatabases(s.ID)
	if err != nil {
		// Failing open would hand the server the cluster network; failing closed
		// costs it a database it can reconnect to on the next resync.
		log.Printf("network policy for %s: list databases (its database egress is denied until this clears): %v", s.Slug, err)
		return peers
	}
	seen := map[string]bool{}
	for i := range dbs {
		h, err := r.Store.GetDatabaseHost(dbs[i].HostID)
		if err != nil {
			continue
		}
		switch {
		case h.Kind == models.DBHostManaged:
			if ns := ManagedDBNamespace(h); ns != "" && !seen[ns] {
				seen[ns] = true
				peers = append(peers, EgressPeer{Namespace: ns})
			}
		default:
			if c := hostCIDR(h.ConnectHost, h.Host); c != "" && !seen[c] {
				seen[c] = true
				peers = append(peers, EgressPeer{CIDR: c})
			}
		}
	}
	return peers
}

// hostCIDR turns an external host into a single-address CIDR when it is written
// as a literal IP. A hostname yields "": it cannot go in a NetworkPolicy, and
// guessing by resolving it here would bake in an answer that changes.
func hostCIDR(candidates ...string) string {
	for _, c := range candidates {
		h := strings.TrimSpace(c)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			if ip.To4() != nil {
				return ip.String() + "/32"
			}
			return ip.String() + "/128"
		}
	}
	return ""
}

// ensureSecret creates/updates the per-server Secret, skipping the write when
// the stored contents already match. (Secret.stringData is write-only, so we
// compare against the decoded Data; this avoids SSA's stringData pitfalls.)
func (r *Reconciler) ensureSecret(ctx context.Context, want *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if secretDataEqual(existing.Data, want.StringData) {
		return nil
	}
	existing.Labels = mergeLabels(existing.Labels, want.Labels)
	existing.Data = nil
	existing.StringData = want.StringData
	return r.Client.Update(ctx, existing)
}

func secretDataEqual(data map[string][]byte, want map[string]string) bool {
	if len(data) != len(want) {
		return false
	}
	for k, v := range want {
		if string(data[k]) != v {
			return false
		}
	}
	return true
}

// updateStatus reads the workload + pods and writes an observed status to the DB,
// including crash detection.
func (r *Reconciler) updateStatus(ctx context.Context, s *models.Server, t *models.Template) error {
	eps, addr := r.endpointsFor(ctx, s, t)
	st := models.Status{Endpoints: eps, Address: addr}

	switch {
	case s.DesiredState == models.StateSuspended:
		st.Phase = models.PhaseSuspended
	case s.DesiredState == models.StateStopped:
		st.Phase = models.PhaseStopped
	case s.Hibernated:
		st.Phase = models.PhaseHibernated
	default: // Running
		h := r.inspectPods(ctx, s.Namespace, s.Slug)
		st.CrashCount = h.restarts
		switch {
		// The install is checked before readiness: a pod whose init container
		// keeps failing is never ready, and reporting that as "Starting" is what
		// hid the failure. Ready still wins over "installing", since a running
		// pod has by definition finished its init containers.
		case h.installFailed:
			st.Phase = models.PhaseError
			st.Message = installFailureMessage(h)
		case r.deploymentReady(ctx, s.Namespace):
			st.Phase = models.PhaseRunning
		case h.crashloop:
			st.Phase = models.PhaseCrashed
			st.Message = h.msg
		case h.installing:
			st.Phase = models.PhaseInstalling
			st.Message = "running " + h.installStep
		default:
			st.Phase = models.PhaseStarting
		}
		r.emitRestartEvents(s, s.Status, h, st.CrashCount)
	}

	// Surface the silent no-op: enabling SFTP needs a system image (the SFTP
	// binary ships in it), otherwise the sidecar is never added and the toggle
	// looks active while nothing serves.
	if s.SFTP.Enabled && r.ActivatorImage == "" {
		warn := "SFTP is enabled but no system image is configured (set QUETZAL_IMAGE); the SFTP sidecar will not start"
		if st.Message == "" {
			st.Message = warn
		} else {
			st.Message += "; " + warn
		}
	}

	r.emitTransition(s, s.Status.Phase, st)
	return r.Store.UpdateServerStatus(s.ID, st)
}

// installFailureMessage says which step failed, with what code, and quotes what
// the step wrote if Kubernetes captured it. The point is that the message alone
// is enough to act on: "install exited with code 7" sends someone to the egg's
// script, not to a support forum.
func installFailureMessage(h podHealth) string {
	msg := fmt.Sprintf("%s failed (exit code %d)", h.installStep, h.installExit)
	if h.installMessage != "" {
		msg += ": " + h.installMessage
	}
	return msg + " — see the install log for the output"
}

// emitTransition records an event when a server crosses into a phase worth
// notifying about. It only covers transitions the API can't already see
// (the controller observes crashes, readiness and idle-hibernation); power
// actions are emitted by the API itself, so they are not duplicated here.
func (r *Reconciler) emitTransition(s *models.Server, old models.Phase, st models.Status) {
	if old == st.Phase {
		return
	}
	switch st.Phase {
	case models.PhaseRunning:
		r.emitEvent(s, models.EventServerRunning, "is up and running")
	case models.PhaseError:
		// Reaching Error means the install or the config render gave up. Say so
		// once, on the transition, so a notification channel is told rather than
		// leaving it to whoever next opens the panel.
		r.emitEvent(s, models.EventServerInstallFailed, st.Message)
	case models.PhaseCrashed:
		msg := "crashed"
		if st.CrashCount > 0 {
			msg = fmt.Sprintf("crashed (%d restarts)", st.CrashCount)
		}
		if st.Message != "" {
			msg += ": " + st.Message
		}
		r.emitEvent(s, models.EventServerCrashed, msg)
	case models.PhaseHibernated:
		r.emitEvent(s, models.EventServerHibernated, "hibernated after inactivity")
	}
}

// emitRestartEvents records an event when a container restart is newly observed,
// so a server that keeps dying and coming back (classically an OOM loop) is
// visible in the activity log instead of restarting silently. It fires on any
// growth in the cumulative restart count, and also when the count resets to a
// positive value (a fresh pod that already restarted before we first saw it).
func (r *Reconciler) emitRestartEvents(s *models.Server, old models.Status, h podHealth, newCount int) {
	increased := newCount > old.CrashCount
	reset := newCount < old.CrashCount && newCount > 0
	if !increased && !reset {
		return
	}
	switch {
	case h.oomKilled:
		r.emitEvent(s, models.EventServerOOMKilled,
			fmt.Sprintf("ran out of memory (OOMKilled) and was restarted — %d restart(s) so far", newCount))
	case h.crashloop:
		// The crashloop phase transition already emits server.crashed; don't double up.
	case h.exitCode != 0:
		r.emitEvent(s, models.EventServerRestarted,
			fmt.Sprintf("container exited (code %d) and was restarted — %d so far", h.exitCode, newCount))
	default:
		r.emitEvent(s, models.EventServerRestarted,
			fmt.Sprintf("container restarted — %d so far", newCount))
	}
}

// emitEvent appends a server-scoped event (best-effort). The apiserver's
// dispatcher delivers it on its next pass.
func (r *Reconciler) emitEvent(s *models.Server, eventType, message string) {
	_ = r.Store.AddEvent(&models.Event{
		ServerID: s.ID, Type: eventType, Message: s.Slug + ": " + message,
	})
}

func (r *Reconciler) deploymentReady(ctx context.Context, ns string) bool {
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, dep); err != nil {
		return false
	}
	return dep.Status.ReadyReplicas >= 1
}

func (r *Reconciler) deploymentRunning(ctx context.Context, ns string) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Spec.Replicas != nil && *dep.Spec.Replicas > 0, nil
}

// podHealth is what inspectPods observes about a server's pods: cumulative
// container restarts, whether any container is in CrashLoopBackOff, and why the
// last restart happened (so a silent OOM restart loop can be surfaced).
type podHealth struct {
	restarts   int
	crashloop  bool
	msg        string
	oomKilled  bool
	termReason string // last termination reason, e.g. "OOMKilled", "Error"
	exitCode   int32  // last termination exit code (0 when unknown)

	// The init containers — the egg's install step and the config render — are
	// the part nothing used to look at. A failure there never reaches
	// ContainerStatuses (the main container has not started), so the server sat
	// on "Starting" for good: no phase, no message, no event, while the answer
	// was in the init container's log the whole time.
	installing     bool
	installFailed  bool
	installStep    string // the init container concerned
	installExit    int32
	installMessage string
}

// inspectPods sums container restarts, detects CrashLoopBackOff, and records the
// most recent termination (reason + exit code) — including OOMKilled, which
// otherwise leaves no trace when the container restarts fast enough to never
// enter CrashLoopBackOff.
func (r *Reconciler) inspectPods(ctx context.Context, ns, slug string) podHealth {
	var h podHealth
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{serverLabel: slug}); err != nil {
		return h
	}
	note := func(term *corev1.ContainerStateTerminated) {
		if term == nil {
			return
		}
		h.termReason = term.Reason
		h.exitCode = term.ExitCode
		if term.Reason == "OOMKilled" {
			h.oomKilled = true
		}
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			h.restarts += int(cs.RestartCount)
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				h.crashloop = true
				h.msg = cs.State.Waiting.Message
				if h.msg == "" {
					h.msg = "container in CrashLoopBackOff"
				}
			}
			// The previous run's exit explains a restart even once the container is
			// back up; a container terminated right now is captured too.
			note(cs.LastTerminationState.Terminated)
			note(cs.State.Terminated)
		}
		noteInit(&h, pods.Items[i].Status.InitContainerStatuses)
	}
	return h
}

// noteInit reads the init containers: the install step and the config render.
// Exit 0 means done, and a pod that has started its main container reports them
// all that way — so only a non-zero exit, or a back-off after one, is a failure.
// Anything still pending means the install is in progress, which is worth saying
// too: a large modpack takes minutes and used to be indistinguishable from a
// server that simply would not start.
func noteInit(h *podHealth, statuses []corev1.ContainerStatus) {
	for _, cs := range statuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
			h.installFailed, h.installStep = true, cs.Name
			h.installExit = cs.State.Terminated.ExitCode
			h.installMessage = strings.TrimSpace(cs.State.Terminated.Message)
		case cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff":
			// Kubernetes retries a failed init container in place; while it waits
			// between attempts the exit code is only on the previous state.
			h.installFailed, h.installStep = true, cs.Name
			if t := cs.LastTerminationState.Terminated; t != nil {
				h.installExit = t.ExitCode
				h.installMessage = strings.TrimSpace(t.Message)
			}
		case cs.State.Terminated == nil:
			h.installing = true
			if h.installStep == "" {
				h.installStep = cs.Name
			}
		}
	}
}

// endpointsFor computes the reachable addresses for a server and picks a primary
// one (the primary port, or the sole port). External exposure (NodePort/
// LoadBalancer) yields node/LB addresses; otherwise the in-cluster DNS names.
func (r *Reconciler) endpointsFor(ctx context.Context, s *models.Server, t *models.Template) (eps []string, addr string) {
	ports := serverPorts(s, t)
	add := func(p models.PortSpec, ep string) {
		eps = append(eps, ep)
		if addr == "" && (p.Primary || len(ports) == 1) {
			addr = ep
		}
	}

	switch s.Expose.ServiceType() {
	case models.ExposeNodePort:
		host := r.endpointHost(ctx)
		if host == "" {
			host = "<node-ip>"
		}
		for _, p := range ports {
			if p.NodePort == 0 {
				continue
			}
			add(p, fmt.Sprintf("%s:%d", host, p.NodePort))
		}
	case models.ExposeLoadBalancer:
		host := r.loadBalancerAddress(ctx, s.Namespace)
		if host == "" {
			break // not yet provisioned
		}
		for _, p := range ports {
			add(p, fmt.Sprintf("%s:%d", host, p.Port))
		}
	default: // ClusterIP
		for _, p := range ports {
			add(p, fmt.Sprintf("%s.%s.svc.cluster.local:%d", workloadName, s.Namespace, p.Port))
		}
	}
	if addr == "" && len(eps) > 0 {
		addr = eps[0]
	}
	return eps, addr
}

// endpointHost is the host published in a server's external NodePort endpoints:
// this cluster's own hostname if it has one, else the panel-wide setting, else
// the detected node address. Letting the admin pin a hostname means players see
// a stable, memorable address instead of the raw node IP.
func (r *Reconciler) endpointHost(ctx context.Context) string {
	if h := store.EndpointHostFor(r.Store, r.ClusterID); h != "" {
		return h
	}
	return r.firstNodeAddress(ctx)
}

// firstNodeAddress returns a usable node address, preferring an ExternalIP and
// falling back to an InternalIP.
func (r *Reconciler) firstNodeAddress(ctx context.Context) string {
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return ""
	}
	return NodeAddress(nodes.Items)
}

// NodeAddress picks the address to reach a cluster on: the first ExternalIP,
// falling back to the first InternalIP, and "" when neither exists. Exported so
// the apiserver derives the same address as the controller from its own client,
// instead of keeping a second copy of the preference order.
func NodeAddress(nodes []corev1.Node) string {
	var internal string
	for i := range nodes {
		for _, a := range nodes[i].Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				if a.Address != "" {
					return a.Address
				}
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
	}
	return internal
}

// loadBalancerAddress returns the Service's external LB address once assigned.
func (r *Reconciler) loadBalancerAddress(ctx context.Context, ns string) string {
	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, svc); err != nil {
		return ""
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}

func mergeLabels(into, from map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}
	for k, v := range from {
		into[k] = v
	}
	return into
}

// isConsoleStop reports whether a template's stop command should be written to
// the container's stdin for a graceful stop. Pterodactyl encodes a signal-based
// stop as a caret token (e.g. "^C" = SIGINT, used by some proxies/limbos); that
// isn't console input, so writing the literal "^C" does nothing. For those we
// skip the stdin write and let pod termination deliver SIGTERM (+ the grace
// period), which those servers handle as a clean shutdown.
func isConsoleStop(cmd string) bool {
	return cmd != "" && !strings.HasPrefix(cmd, "^")
}
