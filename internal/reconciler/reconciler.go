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
	// A Service requires at least one port; skip it for portless servers.
	if len(serverPorts(srv, tmpl)) > 0 {
		if err := r.ensureService(ctx, srv, tmpl); err != nil {
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

func (r *Reconciler) ensureService(ctx context.Context, s *models.Server, t *models.Template) error {
	return r.apply(ctx, BuildService(s, t))
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
	st := models.Status{Endpoints: endpoints(s, t)}

	switch s.DesiredState {
	case models.StateSuspended:
		st.Phase = models.PhaseSuspended
	case models.StateStopped:
		st.Phase = models.PhaseStopped
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

func endpoints(s *models.Server, t *models.Template) []string {
	var eps []string
	for _, p := range serverPorts(s, t) {
		eps = append(eps, fmt.Sprintf("%s.%s.svc.cluster.local:%d", workloadName, s.Namespace, p.Port))
	}
	return eps
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
