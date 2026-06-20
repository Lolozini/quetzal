//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2EExposeTransitions verifies the Phase 2 exposure path end to end,
// including the riskiest case: switching a live Service between types via
// server-side apply (which must clear nodePort + externalTrafficPolicy when
// going back to ClusterIP, atomically, or the apply is rejected).
func TestE2EExposeTransitions(t *testing.T) {
	ctx, c, st, rec := setup(t)

	mcTmpl, err := st.GetTemplateBySlug("minecraft-paper")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}

	srv := &models.Server{
		Slug: "e2e-expose", DisplayName: "expose", TemplateID: mcTmpl.ID,
		TemplateVersion: mcTmpl.Version, Image: pauseImage,
		Namespace: reconciler.NamespaceFor("e2e-expose"), DesiredState: models.StateRunning,
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Ports:   mcTmpl.Ports, // default exposure: ClusterIP
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	// 1) Default: ClusterIP, no node port, no externalTrafficPolicy.
	svc := getService(ctx, t, c, srv.Namespace)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("initial type = %q, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" || svc.Spec.Ports[0].NodePort != 0 {
		t.Fatalf("ClusterIP must have no ETP/nodePort, got etp=%q np=%d",
			svc.Spec.ExternalTrafficPolicy, svc.Spec.Ports[0].NodePort)
	}

	// 2) Switch to NodePort: allocate from the pool and reconcile.
	np, err := st.AllocateNodePort(srv.ID, mcTmpl.Ports[0].Name, 0, 0)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	ports := append([]models.PortSpec(nil), mcTmpl.Ports...)
	ports[0].NodePort = np
	if err := st.UpdateServerNetworking(srv.ID, models.Expose{Type: models.ExposeNodePort}, ports); err != nil {
		t.Fatalf("set nodeport: %v", err)
	}
	reconcileOnce(ctx, t, rec, srv.ID)

	svc = getService(ctx, t, c, srv.Namespace)
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("type = %q, want NodePort", svc.Spec.Type)
	}
	if svc.Spec.Ports[0].NodePort != np {
		t.Errorf("nodePort = %d, want %d", svc.Spec.Ports[0].NodePort, np)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", svc.Spec.ExternalTrafficPolicy)
	}
	if got, _ := st.GetServer(srv.ID); !strings.HasSuffix(got.Status.Address, fmt.Sprintf(":%d", np)) {
		t.Errorf("status address = %q, want <nodeIP>:%d", got.Status.Address, np)
	}

	// 3) Switch back to ClusterIP: this is the SSA transition that must clear
	// nodePort + externalTrafficPolicy in a single apply.
	if err := st.ReleaseServerPorts(srv.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	cleared := append([]models.PortSpec(nil), mcTmpl.Ports...) // NodePort = 0
	if err := st.UpdateServerNetworking(srv.ID, models.Expose{Type: models.ExposeClusterIP}, cleared); err != nil {
		t.Fatalf("set clusterip: %v", err)
	}
	reconcileOnce(ctx, t, rec, srv.ID)

	svc = getService(ctx, t, c, srv.Namespace)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("type after revert = %q, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy not cleared: %q", svc.Spec.ExternalTrafficPolicy)
	}
	if svc.Spec.Ports[0].NodePort != 0 {
		t.Errorf("nodePort not cleared: %d", svc.Spec.Ports[0].NodePort)
	}
}

func reconcileOnce(ctx context.Context, t *testing.T, rec *reconciler.Reconciler, id uint) {
	t.Helper()
	if err := rec.ReconcileServer(ctx, id); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getService(ctx context.Context, t *testing.T, c client.Client, ns string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "server"}, svc); err != nil {
		t.Fatalf("service missing: %v", err)
	}
	return svc
}
