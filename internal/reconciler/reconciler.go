// Package reconciler projects servers (the DB source of truth) into native
// Kubernetes objects, and writes observed status back to the DB.
package reconciler

import (
	"context"
	"fmt"
	"log"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	// NodePortMin/NodePortMax bound the node-port pool the SFTP Service draws
	// from (0 = the store's defaults). Same pool as the game ports, so SFTP and
	// game allocations never collide. Injected by the controller.
	NodePortMin int32
	NodePortMax int32
}

// New returns a Reconciler.
func New(c client.Client, s *store.Store) *Reconciler {
	return &Reconciler{Client: c, Store: s}
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

	if err := r.ensureNamespace(ctx, srv); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
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
func (r *Reconciler) GCOrphanNamespaces(ctx context.Context, validSlugs map[string]bool) error {
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
		return nil
	})
	return err
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
	return r.apply(ctx, BuildNetworkPolicy(s, t))
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
		case h.crashloop:
			st.Phase = models.PhaseCrashed
			st.Message = h.msg
		case r.deploymentReady(ctx, s.Namespace):
			st.Phase = models.PhaseRunning
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
	}
	return h
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
// the admin-configured DNS name (SettingEndpointHost) when set, otherwise the
// detected node address. Letting the admin pin a hostname means players see a
// stable, memorable address instead of the raw node IP.
func (r *Reconciler) endpointHost(ctx context.Context) string {
	if h, err := r.Store.GetSetting(store.SettingEndpointHost); err == nil {
		if h = strings.TrimSpace(h); h != "" {
			return h
		}
	}
	return r.firstNodeAddress(ctx)
}

// firstNodeAddress returns a usable node address, preferring an ExternalIP and
// falling back to an InternalIP.
func (r *Reconciler) firstNodeAddress(ctx context.Context) string {
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil || len(nodes.Items) == 0 {
		return ""
	}
	var internal string
	for i := range nodes.Items {
		for _, a := range nodes.Items[i].Status.Addresses {
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
