// Package reconciler projects servers (the DB source of truth) into native
// Kubernetes objects, and writes observed status back to the DB.
package reconciler

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	if err := r.ensureDeployment(ctx, srv, tmpl); err != nil {
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

func (r *Reconciler) ensureDeployment(ctx context.Context, s *models.Server, t *models.Template) error {
	want := BuildDeployment(s, t)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = mergeLabels(dep.Labels, want.Labels)
		dep.Spec = want.Spec
		return nil
	})
	return err
}

func (r *Reconciler) ensureService(ctx context.Context, s *models.Server, t *models.Template) error {
	want := BuildService(s, t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeLabels(svc.Labels, want.Labels)
		// Preserve immutable fields (ClusterIP); only set what we manage.
		svc.Spec.Type = want.Spec.Type
		svc.Spec.Selector = want.Spec.Selector
		svc.Spec.Ports = want.Spec.Ports
		return nil
	})
	return err
}

func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, s *models.Server, t *models.Template) error {
	want := BuildNetworkPolicy(s, t)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = mergeLabels(np.Labels, want.Labels)
		np.Spec = want.Spec
		return nil
	})
	return err
}

// updateStatus reads the Deployment and writes an observed status to the DB.
func (r *Reconciler) updateStatus(ctx context.Context, s *models.Server, t *models.Template) error {
	st := models.Status{Endpoints: endpoints(s, t)}

	switch s.DesiredState {
	case models.StateSuspended:
		st.Phase = models.PhaseSuspended
	case models.StateStopped:
		st.Phase = models.PhaseStopped
	default: // Running
		dep := &appsv1.Deployment{}
		key := client.ObjectKey{Namespace: s.Namespace, Name: workloadName}
		if err := r.Client.Get(ctx, key, dep); err != nil {
			if apierrors.IsNotFound(err) {
				st.Phase = models.PhaseStarting
			} else {
				return err
			}
		} else if dep.Status.ReadyReplicas >= 1 {
			st.Phase = models.PhaseRunning
		} else {
			st.Phase = models.PhaseStarting
		}
	}

	return r.Store.UpdateServerStatus(s.ID, st)
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
