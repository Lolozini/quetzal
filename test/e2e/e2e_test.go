//go:build e2e

// Package e2e exercises the full reconcile path against a real Kubernetes
// cluster (a disposable kind cluster in CI). It is excluded from normal
// `go test ./...` by the build tag and requires a working kubeconfig
// (KUBECONFIG or the default context).
package e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

const pauseImage = "registry.k8s.io/pause:3.10"

func setup(t *testing.T) (context.Context, client.Client, *store.Store, *reconciler.Reconciler) {
	t.Helper()
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config (is KUBECONFIG set to a cluster?): %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "e2e.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return context.Background(), c, st, reconciler.New(c, st)
}

// reconcileUntilRunning repeatedly reconciles until the DB status reaches the
// Running phase (a pod is actually ready) or the timeout elapses.
func reconcileUntilRunning(ctx context.Context, t *testing.T, rec *reconciler.Reconciler, st *store.Store, id uint) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := rec.ReconcileServer(ctx, id); err != nil {
			t.Logf("reconcile (retrying): %v", err)
			return false, nil
		}
		srv, err := st.GetServer(id)
		if err != nil {
			return false, err
		}
		return srv.Status.Phase == models.PhaseRunning, nil
	})
	if err != nil {
		t.Fatalf("server %d never reached Running: %v", id, err)
	}
}

func TestE2ELifecycle(t *testing.T) {
	ctx, c, st, rec := setup(t)

	genTmpl, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	mcTmpl, err := st.GetTemplateBySlug("minecraft-paper")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}

	// Portless server (alpine, runs the startup shim).
	portless := &models.Server{
		Slug: "e2e-portless", DisplayName: "portless", TemplateID: genTmpl.ID,
		TemplateVersion: genTmpl.Version, Image: defaultImage(genTmpl),
		Namespace: reconciler.NamespaceFor("e2e-portless"), DesiredState: models.StateRunning,
		Env: map[string]string{"MESSAGE": "hi"}, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	// Ported server (pause image keeps the pod up; gets a Service).
	ported := &models.Server{
		Slug: "e2e-ported", DisplayName: "ported", TemplateID: mcTmpl.ID,
		TemplateVersion: mcTmpl.Version, Image: pauseImage,
		Namespace: reconciler.NamespaceFor("e2e-ported"), DesiredState: models.StateRunning,
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"}, Ports: mcTmpl.Ports,
	}
	for _, s := range []*models.Server{portless, ported} {
		if err := st.CreateServer(s); err != nil {
			t.Fatalf("create server %s: %v", s.Slug, err)
		}
		t.Cleanup(func() { _ = rec.DeleteServer(ctx, s) }) // best-effort teardown
	}

	reconcileUntilRunning(ctx, t, rec, st, portless.ID)
	reconcileUntilRunning(ctx, t, rec, st, ported.ID)

	// Portless: namespace + deployment + networkpolicy + running pod, but NO service.
	assertExists(ctx, t, c, &appsv1.Deployment{}, portless.Namespace, "server")
	assertExists(ctx, t, c, &networkingv1.NetworkPolicy{}, portless.Namespace, "quetzal-default")
	assertNotExists(ctx, t, c, &corev1.Service{}, portless.Namespace, "server")
	assertPodRunning(ctx, t, c, portless.Namespace)

	// Security: the namespace carries a ResourceQuota (server-side applied) and it
	// does not prevent the pod from running.
	assertExists(ctx, t, c, &corev1.ResourceQuota{}, portless.Namespace, "quetzal-quota")

	// Ported: a Service with the game port exists.
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ported.Namespace, Name: "server"}, svc); err != nil {
		t.Fatalf("ported service missing: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 {
		t.Fatalf("service ports = %+v, want one :25565", svc.Spec.Ports)
	}

	// Stop the portless server -> deployment scales to 0.
	portless.DesiredState = models.StateStopped
	if err := st.UpdateServer(portless); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := rec.ReconcileServer(ctx, portless.ID); err != nil {
		t.Fatalf("reconcile stop: %v", err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: portless.Namespace, Name: "server"}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Fatalf("stopped replicas = %v, want 0", dep.Spec.Replicas)
	}

	// Delete the ported server -> GC removes its namespace.
	if err := st.DeleteServer(ported.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := rec.GCOrphanNamespaces(ctx, map[string]bool{portless.Slug: true}); err != nil {
		t.Fatalf("gc: %v", err)
	}
	assertNamespaceGone(ctx, t, c, ported.Namespace)
}

func defaultImage(t *models.Template) string {
	for _, img := range t.Images {
		if img.Default {
			return img.Ref
		}
	}
	return t.Images[0].Ref
}

func assertExists(ctx context.Context, t *testing.T, c client.Client, obj client.Object, ns, name string) {
	t.Helper()
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Errorf("expected %T %s/%s to exist: %v", obj, ns, name, err)
	}
}

func assertNotExists(ctx context.Context, t *testing.T, c client.Client, obj client.Object, ns, name string) {
	t.Helper()
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj)
	if err == nil {
		t.Errorf("expected %T %s/%s to NOT exist", obj, ns, name)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error checking %T %s/%s: %v", obj, ns, name, err)
	}
}

func assertPodRunning(ctx context.Context, t *testing.T, c client.Client, ns string) {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no pods in %s", ns)
	}
	if phase := pods.Items[0].Status.Phase; phase != corev1.PodRunning {
		t.Errorf("pod phase = %s, want Running", phase)
	}
}

func assertNamespaceGone(ctx context.Context, t *testing.T, c client.Client, ns string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		e := c.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{})
		return apierrors.IsNotFound(e), nil
	})
	if err != nil {
		t.Errorf("namespace %s was not garbage-collected: %v", ns, err)
	}
}
