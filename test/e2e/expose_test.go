//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// TestE2ENodePortExpose verifies the Phase 2 exposure path: a server published
// via NodePort gets a NodePort Service with the pool-allocated port,
// externalTrafficPolicy: Local, and an external address in its status.
func TestE2ENodePortExpose(t *testing.T) {
	ctx, c, st, rec := setup(t)

	mcTmpl, err := st.GetTemplateBySlug("minecraft-paper")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}

	srv := &models.Server{
		Slug: "e2e-nodeport", DisplayName: "nodeport", TemplateID: mcTmpl.ID,
		TemplateVersion: mcTmpl.Version, Image: pauseImage,
		Namespace: reconciler.NamespaceFor("e2e-nodeport"), DesiredState: models.StateRunning,
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
		Ports:   mcTmpl.Ports,
		Expose:  models.Expose{Type: models.ExposeNodePort},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })

	// Allocate a stable node port from the pool, as the API layer does.
	np, err := st.AllocateNodePort(srv.ID, mcTmpl.Ports[0].Name, 0, 0)
	if err != nil {
		t.Fatalf("allocate node port: %v", err)
	}
	ports := append([]models.PortSpec(nil), mcTmpl.Ports...)
	ports[0].NodePort = np
	if err := st.UpdateServerNetworking(srv.ID, srv.Expose, ports); err != nil {
		t.Fatalf("update networking: %v", err)
	}

	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: "server"}, svc); err != nil {
		t.Fatalf("service missing: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("service type = %q, want NodePort", svc.Spec.Type)
	}
	if svc.Spec.Ports[0].NodePort != np {
		t.Errorf("service nodePort = %d, want allocated %d", svc.Spec.Ports[0].NodePort, np)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", svc.Spec.ExternalTrafficPolicy)
	}

	// Status must advertise an external address of the form <nodeIP>:<nodePort>.
	got, err := st.GetServer(srv.ID)
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	wantSuffix := fmt.Sprintf(":%d", np)
	if !strings.HasSuffix(got.Status.Address, wantSuffix) || got.Status.Address == wantSuffix {
		t.Errorf("status address = %q, want <nodeIP>%s", got.Status.Address, wantSuffix)
	}
}
