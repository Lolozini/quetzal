//go:build e2e

package e2e

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

// A managed database runs as the image's mysql user from the start, with every
// capability dropped and nothing to escalate to. The official entrypoint
// supports it and the image's health check answers without switching user,
// but only a real MariaDB on a real volume says so: ready means the probe,
// run as that user, found InnoDB up and took a connection.
func TestE2EManagedDatabaseRunsAsMySQL(t *testing.T) {
	ctx, c, st, rec := setup(t)
	h := &models.DatabaseHost{Name: "e2e-managed", Kind: models.DBHostManaged, Image: "mariadb:12.3", StorageSize: "1Gi"}
	if err := st.CreateDatabaseHost(h, "e2e-root-password"); err != nil {
		t.Fatalf("create host: %v", err)
	}
	h.Namespace = h.ManagedNamespace()
	h.Host = reconciler.ManagedDBServiceHost(h)
	if err := st.UpdateDatabaseHost(h, nil); err != nil {
		t.Fatalf("update host: %v", err)
	}
	ns := h.Namespace
	t.Cleanup(func() {
		_ = c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	})

	deadline := time.Now().Add(5 * time.Minute)
	for {
		if err := rec.ReconcileDatabaseHosts(ctx); err != nil {
			t.Fatalf("reconcile database hosts: %v", err)
		}
		dep := &appsv1.Deployment{}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: reconciler.ManagedDBServiceName}, dep)
		if err == nil && dep.Status.ReadyReplicas == 1 {
			break
		}
		if time.Now().After(deadline) {
			pods := &corev1.PodList{}
			_ = c.List(ctx, pods, client.InNamespace(ns))
			for _, p := range pods.Items {
				t.Logf("pod %s: phase %s, conditions %+v, containers %+v", p.Name, p.Status.Phase, p.Status.Conditions, p.Status.ContainerStatuses)
			}
			t.Fatal("the managed database never became ready")
		}
		time.Sleep(5 * time.Second)
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ns)); err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods: %v (%d)", err, len(pods.Items))
	}
	for _, p := range pods.Items {
		sc := p.Spec.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 999 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("pod %s runs as %+v, want the mysql user", p.Name, sc)
		}
	}
}
