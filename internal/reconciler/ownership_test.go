package reconciler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

func TestEnsureNamespaceRejectsForeignOwnership(t *testing.T) {
	ctx := context.Background()
	for _, mine := range []string{"inst-mine", ""} {
		t.Run("owner-"+mine, func(t *testing.T) {
			ns := nsWithLabels("quetzal-srv-foreign", map[string]string{
				managedByLabel: managedByValue, serverLabel: "foreign", InstanceLabel: "inst-foreign",
			})
			cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ns).Build()
			r := &Reconciler{Client: cl, InstanceID: mine}
			srv := &models.Server{Slug: "foreign", Namespace: ns.Name}
			if err := r.ensureNamespace(ctx, srv); err == nil {
				t.Error("accepted another instance's namespace")
			}
			var got corev1.Namespace
			if err := cl.Get(ctx, client.ObjectKey{Name: ns.Name}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Labels[InstanceLabel] != "inst-foreign" {
				t.Error("foreign ownership was overwritten")
			}
			if err := r.GCOrphanNamespaces(ctx, map[string]bool{}); err != nil {
				t.Fatal(err)
			}
			if !exists(ctx, t, cl, ns.Name) {
				t.Error("foreign namespace collected after reconciliation")
			}
		})
	}
}

// Real server-side apply is essential here: an in-memory patch imitation would
// not reproduce namespace ownership being stolen by ForceOwnership.
func TestManagedDBForeignOwnershipLive(t *testing.T) {
	path := os.Getenv("QUETZAL_AUDIT_KUBECONFIG")
	if path == "" {
		t.Skip("set QUETZAL_AUDIT_KUBECONFIG to a disposable Kubernetes cluster")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ns := nsWithLabels(fmt.Sprintf("quetzal-db-audit-%d", time.Now().UnixNano()), map[string]string{
		managedByLabel: managedByValue, dbComponentLabel: dbComponentValue, InstanceLabel: "audit-foreign",
	})
	if err := cl.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	defer cl.Delete(context.Background(), ns)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: dbRootSecret, Namespace: ns.Name}, Data: map[string][]byte{dbRootField: []byte("foreign-password")}}
	if err := cl.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	st := reconStore(t)
	h := &models.DatabaseHost{Name: "audit", Kind: models.DBHostManaged, Namespace: ns.Name, StorageSize: "1Gi"}
	if err := st.CreateDatabaseHost(h, "attacker-password"); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: cl, Store: st, InstanceID: "audit-mine"}
	if err := r.ReconcileDatabaseHosts(ctx); err != nil {
		t.Fatal(err)
	}
	var got corev1.Namespace
	if err := cl.Get(ctx, client.ObjectKey{Name: ns.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[InstanceLabel] != "audit-foreign" {
		t.Error("database namespace ownership was stolen")
	}
	var root corev1.Secret
	if err := cl.Get(ctx, client.ObjectKeyFromObject(secret), &root); err != nil {
		t.Fatal(err)
	}
	if string(root.Data[dbRootField]) != "foreign-password" {
		t.Error("foreign database root password was overwritten")
	}
	if err := r.gcManagedDBNamespaces(ctx, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if !exists(ctx, t, cl, ns.Name) {
		t.Error("foreign database namespace became collectible")
	}
}

func TestDeleteServerRejectsForeignOwnership(t *testing.T) {
	ctx := context.Background()
	ns := nsWithLabels("quetzal-srv-foreign", map[string]string{InstanceLabel: "inst-foreign"})
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ns).Build()
	r := &Reconciler{Client: cl, InstanceID: "inst-mine"}
	if err := r.DeleteServer(ctx, &models.Server{Slug: "foreign", Namespace: ns.Name}); err == nil {
		t.Error("deletion accepted a foreign namespace")
	}
	if !exists(ctx, t, cl, ns.Name) {
		t.Error("deleted another instance's namespace")
	}
}

func TestManagedDBInvalidHistoricalHostDoesNotBlockNext(t *testing.T) {
	st := reconStore(t)
	broken := &models.DatabaseHost{Name: "broken", Kind: models.DBHostManaged, StorageSize: "not-a-quantity"}
	healthy := &models.DatabaseHost{Name: "healthy", Kind: models.DBHostManaged, StorageSize: "1Gi"}
	for _, h := range []*models.DatabaseHost{broken, healthy} {
		if err := st.CreateDatabaseHost(h, "pw"); err != nil {
			t.Fatal(err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	r := &Reconciler{Client: cl, Store: st, InstanceID: "inst-mine"}
	ctx := context.Background()
	if err := r.ReconcileDatabaseHosts(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(ctx, t, cl, broken.ManagedNamespace()) {
		t.Error("invalid host mutated its namespace")
	}
	if !exists(ctx, t, cl, healthy.ManagedNamespace()) {
		t.Error("invalid historical storage prevented the next host from reconciling")
	}
}

func TestManagedDBLegacyCollisionIsNotAdoptedOrCollected(t *testing.T) {
	st := reconStore(t)
	first := &models.DatabaseHost{Name: "first", Kind: models.DBHostManaged, Namespace: "quetzal-db-legacy-collision"}
	if err := st.CreateDatabaseHost(first, "pw"); err != nil {
		t.Fatal(err)
	}
	// Simulate rows written before the store enforced namespace reservations.
	second := *first
	second.ID = 0
	second.Name = "second"
	if err := st.DB().Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	ns := nsWithLabels(first.ManagedNamespace(), map[string]string{dbComponentLabel: dbComponentValue})
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ns).Build()
	r := &Reconciler{Client: cl, Store: st, InstanceID: "inst-mine"}
	ctx := context.Background()
	if err := r.ReconcileDatabaseHosts(ctx); err != nil {
		t.Fatal(err)
	}
	var got corev1.Namespace
	if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[InstanceLabel] != "" || got.Labels[dbHostLabel] != "" || got.DeletionTimestamp != nil {
		t.Fatalf("colliding legacy namespace was mutated: %+v", got.ObjectMeta)
	}
}

func TestOwnedNamespaceRejectsOtherDatabaseHost(t *testing.T) {
	ns := nsWithLabels("quetzal-db-owned", map[string]string{InstanceLabel: "mine", dbHostLabel: "1"})
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(ns).Build()
	r := &Reconciler{Client: cl, InstanceID: "mine"}
	ctx := context.Background()
	if err := r.ensureOwnedNamespace(ctx, ns.Name, map[string]string{dbHostLabel: "2"}); err == nil {
		t.Error("namespace was reassigned to another database host")
	}
	var got corev1.Namespace
	if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[dbHostLabel] != "1" {
		t.Error("database host label was overwritten")
	}
}
