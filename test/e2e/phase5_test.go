//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EInstallScript verifies a template's install script runs once (as an
// init container) and populates the data volume before the main container.
func TestE2EInstallScript(t *testing.T) {
	ctx, _, st, rec := setup(t)
	cfg, _ := ctrlconfig.GetConfig()
	cs, _ := kubernetes.NewForConfig(cfg)

	tmpl := &models.Template{
		Slug: "e2e-install", Name: "e2e install", DataPath: "/data",
		Images:  []models.TemplateImage{{Ref: "alpine:3.20", Default: true}},
		Startup: "echo up; while true; do sleep 5; done",
		Console: models.ConsoleConfig{Type: models.ConsoleAttach},
		Install: &models.InstallScript{Image: "alpine:3.20", Script: "echo hello-from-install > /mnt/server/installed.txt"},
	}
	saved, err := st.UpsertTemplate(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-install", DisplayName: "install", TemplateID: saved.ID, TemplateVersion: saved.Version,
		Image: "alpine:3.20", Namespace: reconciler.NamespaceFor("e2e-install"),
		DesiredState: models.StateRunning, Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	pod, err := console.FindRunningPod(ctx, cs, srv.Namespace, srv.Slug)
	if err != nil {
		t.Fatalf("find pod: %v", err)
	}
	out := execInPod(ctx, t, cs, cfg, srv.Namespace, pod, []string{"sh", "-c", "cat /data/installed.txt"})
	if !bytes.Contains([]byte(out), []byte("hello-from-install")) {
		t.Fatalf("install script did not populate the volume; got %q", out)
	}
}

// TestE2EHibernationScaling verifies the reconciler scales a hibernated server
// to zero and back when woken.
func TestE2EHibernationScaling(t *testing.T) {
	ctx, c, st, rec := setup(t)

	gen, _ := st.GetTemplateBySlug("generic-process")
	srv := &models.Server{
		Slug: "e2e-hib", DisplayName: "hib", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-hib"),
		DesiredState: models.StateRunning, Env: map[string]string{"MESSAGE": "hi"},
		Storage:     models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Hibernation: models.Hibernation{Enabled: true, IdleMinutes: 1},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	// Hibernate -> deployment scales to zero.
	if err := st.SetHibernated(srv.ID, true); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if r := deployReplicas(ctx, t, c, srv.Namespace); r != 0 {
		t.Fatalf("hibernated replicas = %d, want 0", r)
	}
	if got, _ := st.GetServer(srv.ID); got.Status.Phase != models.PhaseHibernated {
		t.Errorf("phase = %q, want Hibernated", got.Status.Phase)
	}

	// Wake -> scales back to one.
	if err := st.Wake(srv.ID, time.Now()); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if err := rec.ReconcileServer(ctx, srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if r := deployReplicas(ctx, t, c, srv.Namespace); r != 1 {
		t.Errorf("woken replicas = %d, want 1", r)
	}
}

func deployReplicas(ctx context.Context, t *testing.T, c client.Client, ns string) int32 {
	t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "server"}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Replicas == nil {
		return -1
	}
	return *dep.Spec.Replicas
}
