//go:build e2e

package e2e

import (
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

func depKey(ns string) client.ObjectKey {
	return client.ObjectKey{Namespace: ns, Name: reconciler.WorkloadName}
}

// TestE2EMultiCluster validates the multi-cluster routing path end-to-end:
// register a cluster by its kubeconfig, then reconcile a server through the
// clients the registry builds from that kubeconfig (rather than the process's
// own config). The kubeconfig used is the test cluster's own, so it exercises
// the full DB-row -> registry -> kubeconfig -> clients -> reconcile chain.
func TestE2EMultiCluster(t *testing.T) {
	ctx, c, st, _ := setup(t)

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("KUBECONFIG not set; multi-cluster test needs an explicit kubeconfig")
	}
	kubeconfig, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}

	// Register the cluster as a "remote" target (encrypted kubeconfig in the DB).
	remote := &models.Cluster{Slug: "e2e-remote", Name: "e2e remote"}
	if err := st.CreateCluster(remote, string(kubeconfig)); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	// The registry must build working clients from the stored kubeconfig.
	reg := cluster.New(st, cluster.Clients{})
	clients, err := reg.For(remote.ID)
	if err != nil {
		t.Fatalf("registry.For(remote): %v", err)
	}
	if _, _, err := cluster.Probe(ctx, clients); err != nil {
		t.Fatalf("probe remote: %v", err)
	}

	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	srv := &models.Server{
		Slug: "e2e-remote-srv", DisplayName: "remote", TemplateID: gen.ID, TemplateVersion: gen.Version,
		Image: defaultImage(gen), Namespace: reconciler.NamespaceFor("e2e-remote-srv"),
		ClusterID: remote.ID, DesiredState: models.StateRunning,
		Env:     map[string]string{"MESSAGE": "hi"},
		Storage: models.Storage{Type: models.StoragePVC, Size: "1Gi"},
	}
	if err := st.CreateServer(srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Reconcile through the registry-resolved client (not the local one).
	rec := reconciler.New(clients.Client, st)
	t.Cleanup(func() { _ = rec.DeleteServer(ctx, srv) })
	reconcileUntilRunning(ctx, t, rec, st, srv.ID)

	// The workload landed on the (registry-built) cluster.
	dep := &appsv1.Deployment{}
	if err := clients.Client.Get(ctx, depKey(srv.Namespace), dep); err != nil {
		t.Fatalf("deployment on remote: %v", err)
	}
	// And it's visible through the local client too (same physical cluster here),
	// confirming the namespace was actually created where we expect.
	if err := c.Get(ctx, depKey(srv.Namespace), &appsv1.Deployment{}); err != nil {
		t.Fatalf("deployment not visible via local client: %v", err)
	}
}
