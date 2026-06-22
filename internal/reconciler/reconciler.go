// Package reconciler projects servers (the DB source of truth) into native
// Kubernetes objects, and writes observed status back to the DB.
package reconciler

import (
	"context"
	"fmt"
	"log"

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

	// Wake-on-connect: when ActivatorImage is set, a hibernated server with
	// wake-on-connect enabled gets a tiny activator that listens on its TCP ports
	// and calls WakeURL to wake it. WakeKey signs the per-server callback token.
	ActivatorImage string
	WakeURL        string
	WakeKey        []byte
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
	if srv.DesiredState != models.StateRunning && tmpl.StopCommand != "" && r.OnStop != nil {
		if running, _ := r.deploymentRunning(ctx, srv.Namespace); running {
			if err := r.OnStop(ctx, srv.Namespace, srv.Slug, tmpl.StopCommand); err != nil {
				log.Printf("graceful stop for %s (continuing to scale down): %v", srv.Slug, err)
			}
		}
	}

	if err := r.ensureDeployment(ctx, srv, tmpl, secretKeys); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	// Wake-on-connect: while hibernated, an activator listens on the server's TCP
	// ports and the Service points at it; otherwise it points at the real
	// workload and any activator is removed.
	activator := r.activatorActive(srv, tmpl)
	if err := r.ensureActivator(ctx, srv, tmpl, activator); err != nil {
		return fmt.Errorf("activator: %w", err)
	}
	// A Service requires at least one port; skip it for portless servers.
	if len(serverPorts(srv, tmpl)) > 0 {
		if err := r.ensureService(ctx, srv, tmpl, activator); err != nil {
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
	return r.apply(ctx, BuildDeployment(s, t, secretKeys))
}

func (r *Reconciler) ensureService(ctx context.Context, s *models.Server, t *models.Template, activator bool) error {
	return r.apply(ctx, BuildService(s, t, activator))
}

// activatorActive reports whether a wake-on-connect activator should currently
// front this server: enabled, hibernated, has a TCP port, and an image to run.
func (r *Reconciler) activatorActive(s *models.Server, t *models.Template) bool {
	if r.ActivatorImage == "" || !s.Hibernated || !s.Hibernation.WakeOnConnect {
		return false
	}
	return hasTCPPort(serverPorts(s, t))
}

// ensureActivator creates the activator Deployment when active, or removes it.
func (r *Reconciler) ensureActivator(ctx context.Context, s *models.Server, t *models.Template, active bool) error {
	if !active {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ActivatorName, Namespace: s.Namespace}}
		if err := r.Client.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	token := crypto.WakeToken(r.WakeKey, s.Slug)
	return r.apply(ctx, BuildActivatorDeployment(s, t, r.ActivatorImage, r.WakeURL, token))
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
		restarts, crashloop, msg := r.inspectPods(ctx, s.Namespace, s.Slug)
		st.CrashCount = restarts
		switch {
		case crashloop:
			st.Phase = models.PhaseCrashed
			st.Message = msg
		case r.deploymentReady(ctx, s.Namespace):
			st.Phase = models.PhaseRunning
		default:
			st.Phase = models.PhaseStarting
		}
	}

	return r.Store.UpdateServerStatus(s.ID, st)
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

// inspectPods sums container restarts and detects CrashLoopBackOff.
func (r *Reconciler) inspectPods(ctx context.Context, ns, slug string) (restarts int, crashloop bool, msg string) {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{serverLabel: slug}); err != nil {
		return 0, false, ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			restarts += int(cs.RestartCount)
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				crashloop = true
				msg = cs.State.Waiting.Message
				if msg == "" {
					msg = "container in CrashLoopBackOff"
				}
			}
		}
	}
	return restarts, crashloop, msg
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
		host := r.firstNodeAddress(ctx)
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
