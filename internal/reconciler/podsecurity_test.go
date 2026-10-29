package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lolozini/quetzal/internal/models"
)

// Every namespace Quetzal owns enforces Pod Security baseline: new ones are
// created with it, ones from before it get it on the next reconcile, and one an
// operator tightened to restricted keeps restricted.
func TestOwnedNamespacesEnforcePodSecurity(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		existing map[string]string // nil: the namespace does not exist yet
		want     string
	}{
		"new":        {nil, "baseline"},
		"unlabelled": {map[string]string{managedByLabel: managedByValue, serverLabel: "s"}, "baseline"},
		"privileged": {map[string]string{managedByLabel: managedByValue, serverLabel: "s", PodSecurityLabel: "privileged"}, "baseline"},
		"tightened":  {map[string]string{managedByLabel: managedByValue, serverLabel: "s", PodSecurityLabel: "restricted"}, "restricted"},
		// Pinned to an old release, baseline let through what was added since.
		"pinned": {map[string]string{managedByLabel: managedByValue, serverLabel: "s", PodSecurityLabel: "baseline", PodSecurityVersionLabel: "v1.18"}, "baseline"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tc.existing != nil {
				b = b.WithObjects(nsWithLabels("quetzal-srv-s", tc.existing))
			}
			cl := b.Build()
			r := &Reconciler{Client: cl, InstanceID: "inst"}
			if err := r.ensureNamespace(ctx, &models.Server{Slug: "s", Namespace: "quetzal-srv-s"}); err != nil {
				t.Fatal(err)
			}
			var got corev1.Namespace
			if err := cl.Get(ctx, client.ObjectKey{Name: "quetzal-srv-s"}, &got); err != nil {
				t.Fatal(err)
			}
			if l := got.Labels[PodSecurityLabel]; l != tc.want {
				t.Errorf("%s = %q, want %q", PodSecurityLabel, l, tc.want)
			}
			if v, ok := got.Labels[PodSecurityVersionLabel]; ok && v != "latest" {
				t.Errorf("%s = %q, want unset or latest", PodSecurityVersionLabel, v)
			}
		})
	}

	if l := BuildNamespace(&models.Server{Slug: "s", Namespace: "quetzal-srv-s"}).Labels[PodSecurityLabel]; l != "baseline" {
		t.Errorf("BuildNamespace: %s = %q, want baseline", PodSecurityLabel, l)
	}
	objs, err := buildManagedDB(&models.DatabaseHost{ID: 3, Kind: models.DBHostManaged}, "pw", "inst")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if ns, ok := o.(*corev1.Namespace); ok && ns.Labels[PodSecurityLabel] != "baseline" {
			t.Errorf("managed database namespace: %s = %q, want baseline", PodSecurityLabel, ns.Labels[PodSecurityLabel])
		}
	}
}
