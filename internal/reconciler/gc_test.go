package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

func nsWithLabels(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func exists(ctx context.Context, t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var got corev1.Namespace
	err := c.Get(ctx, types.NamespacedName{Name: name}, &got)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got.DeletionTimestamp == nil
}

// "Orphan" means "no server row in *my* database", so a second control plane
// sharing the cluster sees the first one's namespaces as orphans. Collection
// must therefore skip anything another instance owns, while still reclaiming
// its own and adopting namespaces created before the ownership label existed.
func TestGCOrphanNamespacesRespectsOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	const mine, theirs = "inst-mine", "inst-theirs"

	live := nsWithLabels("quetzal-srv-live", map[string]string{
		managedByLabel: managedByValue, serverLabel: "live", InstanceLabel: mine,
	})
	ownOrphan := nsWithLabels("quetzal-srv-own", map[string]string{
		managedByLabel: managedByValue, serverLabel: "own", InstanceLabel: mine,
	})
	otherInstance := nsWithLabels("quetzal-srv-other", map[string]string{
		managedByLabel: managedByValue, serverLabel: "other", InstanceLabel: theirs,
	})
	legacy := nsWithLabels("quetzal-srv-legacy", map[string]string{
		managedByLabel: managedByValue, serverLabel: "legacy",
	})
	unmanaged := nsWithLabels("kube-system", nil)

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(live, ownOrphan, otherInstance, legacy, unmanaged).Build()
	r := &Reconciler{Client: cl, InstanceID: mine}

	ctx := context.Background()
	if err := r.GCOrphanNamespaces(ctx, map[string]bool{"live": true}); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if !exists(ctx, t, cl, live.Name) {
		t.Error("a live server's namespace was collected")
	}
	if exists(ctx, t, cl, ownOrphan.Name) {
		t.Error("this instance's own orphan was not collected")
	}
	if !exists(ctx, t, cl, otherInstance.Name) {
		t.Error("another control plane's namespace was collected — it would lose that server")
	}
	if exists(ctx, t, cl, legacy.Name) {
		t.Error("an unlabelled (pre-upgrade) orphan was not adopted and collected")
	}
	if !exists(ctx, t, cl, unmanaged.Name) {
		t.Error("a namespace Quetzal does not manage was collected")
	}
}

// Without an instance id nothing can be told apart, so collection must do
// nothing rather than risk deleting another instance's servers.
func TestGCOrphanNamespacesSkippedWithoutInstanceID(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	orphan := nsWithLabels("quetzal-srv-x", map[string]string{
		managedByLabel: managedByValue, serverLabel: "x",
	})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan).Build()
	r := &Reconciler{Client: cl}

	ctx := context.Background()
	if err := r.GCOrphanNamespaces(ctx, map[string]bool{}); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if !exists(ctx, t, cl, orphan.Name) {
		t.Error("collected a namespace with no instance id to compare against")
	}
}

// Namespaces must carry the owning instance so collection can tell them apart,
// and an existing namespace created before the label is adopted in place.
func TestEnsureNamespaceStampsOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	legacy := nsWithLabels("quetzal-srv-old", map[string]string{
		managedByLabel: managedByValue, serverLabel: "old",
	})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	r := &Reconciler{Client: cl, InstanceID: "inst-1"}
	ctx := context.Background()

	// A brand-new namespace.
	if err := r.ensureNamespace(ctx, &models.Server{Slug: "new", Namespace: "quetzal-srv-new"}); err != nil {
		t.Fatalf("ensure new: %v", err)
	}
	// And one that predates the label.
	if err := r.ensureNamespace(ctx, &models.Server{Slug: "old", Namespace: "quetzal-srv-old"}); err != nil {
		t.Fatalf("ensure legacy: %v", err)
	}
	for _, name := range []string{"quetzal-srv-new", "quetzal-srv-old"} {
		var got corev1.Namespace
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got.Labels[InstanceLabel] != "inst-1" {
			t.Errorf("%s instance label = %q, want inst-1", name, got.Labels[InstanceLabel])
		}
		if got.Labels[managedByLabel] != managedByValue {
			t.Errorf("%s lost its managed-by label", name)
		}
	}
}

// The managed-database GC carries the same ownership rule, and the stakes are
// higher: reclaiming another control plane's database namespace destroys that
// data, not just a recreatable game server.
func TestGCManagedDBNamespacesRespectsOwnership(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	const mine, theirs = "inst-mine", "inst-theirs"
	dbLabels := func(instance string) map[string]string {
		l := map[string]string{managedByLabel: managedByValue, dbComponentLabel: dbComponentValue}
		if instance != "" {
			l[InstanceLabel] = instance
		}
		return l
	}
	ownOrphan := nsWithLabels("quetzal-db-own", dbLabels(mine))
	otherInstance := nsWithLabels("quetzal-db-other", dbLabels(theirs))
	legacy := nsWithLabels("quetzal-db-legacy", dbLabels(""))

	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ownOrphan, otherInstance, legacy).Build()
	r := &Reconciler{Client: cl, InstanceID: mine}

	ctx := context.Background()
	if err := r.gcManagedDBNamespaces(ctx, map[string]bool{}); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if exists(ctx, t, cl, ownOrphan.Name) {
		t.Error("this instance's own orphan database namespace was not collected")
	}
	if !exists(ctx, t, cl, otherInstance.Name) {
		t.Error("another control plane's database namespace was collected — that destroys its data")
	}
	if exists(ctx, t, cl, legacy.Name) {
		t.Error("an unlabelled (pre-upgrade) database orphan was not adopted and collected")
	}
}
