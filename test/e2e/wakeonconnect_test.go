//go:build e2e

package e2e

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EWakeOnConnect verifies the wake-on-connect orchestration: while a
// server is hibernated with wake-on-connect, the controller runs an activator
// Deployment and points the Service at it; once the server wakes, the activator
// is removed and the Service points back at the real workload. (The activator
// pod's actual wake callback needs the in-cluster apiserver and is covered by
// unit tests; here we use a stub image and assert the objects.)
func TestE2EWakeOnConnect(t *testing.T) {
	ctx, c, st, rec := setup(t)
	rec.ActivatorImage = pauseImage
	rec.WakeURL = "http://quetzal.invalid/api/internal/wake"
	rec.WakeKey = []byte("test-key")

	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-woc", DisplayName: "woc", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-woc"),
		DesiredState: models.StateRunning, Env: map[string]string{"MESSAGE": "hi"},
		Ports:       []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
		Storage:     models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Hibernation: models.Hibernation{Enabled: true, IdleMinutes: 1, WakeOnConnect: true},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	svcKey := client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.WorkloadName}
	actKey := client.ObjectKey{Namespace: srv.Namespace, Name: reconciler.ActivatorName}

	// Running: Service selects the real workload, no activator.
	svc := &corev1.Service{}
	if err := c.Get(ctx, svcKey, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[reconciler.ServerLabel] != srv.Slug {
		t.Errorf("running selector = %v, want server", svc.Spec.Selector)
	}
	if err := c.Get(ctx, actKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("activator should not exist while running (err=%v)", err)
	}

	// Hibernate -> activator Deployment appears, Service points at it.
	if err := st.SetHibernated(srv.ID, true); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile hibernated: %v", err)
	}
	act := &appsv1.Deployment{}
	if err := c.Get(ctx, actKey, act); err != nil {
		t.Fatalf("activator should exist while hibernated: %v", err)
	}
	if got := act.Spec.Template.Spec.Containers[0].Env; !hasEnv(got, "QUETZAL_PORTS", "25565") {
		t.Errorf("activator env = %v, want QUETZAL_PORTS=25565", got)
	}
	if err := c.Get(ctx, svcKey, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[reconciler.ActivatorLabel] != srv.Slug {
		t.Errorf("hibernated selector = %v, want activator", svc.Spec.Selector)
	}

	// Wake -> activator removed, Service points back at the real workload.
	if err := st.Wake(srv.ID, time.Now()); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile woken: %v", err)
	}
	if err := c.Get(ctx, actKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("activator should be removed after wake (err=%v)", err)
	}
	if err := c.Get(ctx, svcKey, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[reconciler.ServerLabel] != srv.Slug {
		t.Errorf("woken selector = %v, want server", svc.Spec.Selector)
	}
}

func hasEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name {
			return e.Value == value
		}
	}
	return false
}
