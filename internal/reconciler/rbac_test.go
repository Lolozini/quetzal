package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func rbacScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, rbacv1.AddToScheme, authnv1.AddToScheme, authzv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// answerSelfSubjectReview makes the fake cluster say who we are, which the real
// API server does and the fake does not.
func answerSelfSubjectReview(username string) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			if r, ok := obj.(*authnv1.SelfSubjectReview); ok {
				r.Status.UserInfo = authnv1.UserInfo{Username: username}
				return nil // handled; do not persist a virtual resource
			}
			return nil
		},
	}
}

// The control plane grants itself access inside each namespace it creates rather
// than holding it across the cluster, which is what keeps a compromise of it
// away from every other Secret — kube-system included.
func TestEnsureRoleBindingGrantsOnlyInTheNamespaceGiven(t *testing.T) {
	scheme := rbacScheme(t)
	created := []*rbacv1.RoleBinding{}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if r, ok := obj.(*authnv1.SelfSubjectReview); ok {
					r.Status.UserInfo = authnv1.UserInfo{Username: "system:serviceaccount:quetzal:quetzal"}
					return nil
				}
				if rb, ok := obj.(*rbacv1.RoleBinding); ok {
					created = append(created, rb.DeepCopy())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &Reconciler{Client: cl, NamespacedRole: "quetzal-namespaced"}
	ctx := context.Background()

	r.ensureRoleBinding(ctx, "quetzal-srv-demo")
	if len(created) != 1 {
		t.Fatalf("created %d bindings, want 1", len(created))
	}
	rb := created[0]
	if rb.Namespace != "quetzal-srv-demo" {
		t.Errorf("binding landed in %q", rb.Namespace)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "quetzal-namespaced" {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Kind != rbacv1.ServiceAccountKind ||
		rb.Subjects[0].Name != "quetzal" || rb.Subjects[0].Namespace != "quetzal" {
		t.Errorf("subjects = %+v, want the account we authenticate as", rb.Subjects)
	}

	// Nothing was granted anywhere else.
	var all rbacv1.RoleBindingList
	if err := cl.List(ctx, &all); err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range all.Items {
		if all.Items[i].Namespace != "quetzal-srv-demo" {
			t.Errorf("a binding exists in %q", all.Items[i].Namespace)
		}
	}
}

// A cluster registered with an administrator's kubeconfig has no role of ours to
// bind and needs none. Failing to bind there must not stop the reconcile — the
// steps that follow say plainly enough if the access really is missing.
func TestEnsureRoleBindingToleratesNotBeingAllowed(t *testing.T) {
	scheme := rbacScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				if r, ok := obj.(*authnv1.SelfSubjectReview); ok {
					r.Status.UserInfo = authnv1.UserInfo{Username: "system:serviceaccount:quetzal:quetzal"}
					return nil
				}
				return apierrors.NewForbidden(schema.GroupResource{Resource: "rolebindings"}, "quetzal", errors.New("nope"))
			},
		}).Build()
	r := &Reconciler{Client: cl, NamespacedRole: "quetzal-namespaced"}
	r.ensureRoleBinding(context.Background(), "quetzal-srv-demo") // must not panic or block
}

// Not every kubeconfig authenticates as a service account: a client certificate
// or an OIDC user is a User subject, and the binding has to name it correctly or
// grant nobody anything.
func TestEnsureRoleBindingNamesANonServiceAccountSubject(t *testing.T) {
	scheme := rbacScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(answerSelfSubjectReview("kubernetes-admin")).Build()
	r := &Reconciler{Client: cl, NamespacedRole: "quetzal-namespaced"}
	subject, ok := r.identity(context.Background())
	if !ok {
		t.Fatal("identity not resolved")
	}
	if subject.Kind != rbacv1.UserKind || subject.Name != "kubernetes-admin" {
		t.Errorf("subject = %+v, want the User we authenticate as", subject)
	}
}

func TestServiceAccountParts(t *testing.T) {
	for _, c := range []struct{ in, ns, name string }{
		{"system:serviceaccount:quetzal:quetzal", "quetzal", "quetzal"},
		{"system:serviceaccount:a:b", "a", "b"},
	} {
		ns, name, ok := serviceAccountParts(c.in)
		if !ok || ns != c.ns || name != c.name {
			t.Errorf("%q -> %q/%q ok=%v", c.in, ns, name, ok)
		}
	}
	for _, bad := range []string{"kubernetes-admin", "system:serviceaccount:", "system:serviceaccount:onlyns", ""} {
		if _, _, ok := serviceAccountParts(bad); ok {
			t.Errorf("%q parsed as a service account", bad)
		}
	}
}

// A Reconciler with no namespaced role configured — an install predating the
// split — must simply not bind, not fail.
func TestEnsureRoleBindingSkippedWhenUnconfigured(t *testing.T) {
	scheme := rbacScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cl}
	r.ensureRoleBinding(context.Background(), "quetzal-srv-demo")
	var all rbacv1.RoleBindingList
	if err := cl.List(context.Background(), &all); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Items) != 0 {
		t.Errorf("bound %d role(s) without being told which", len(all.Items))
	}
}

// A new binding is enforced only once the API server's RBAC cache has seen it.
// The first reconcile of every new server used to fail on the next step
// ("cannot patch resource resourcequotas") and succeed a tick later. The
// binding is now given a moment to take effect; one that already existed is not.
func TestNewRoleBindingIsWaitedFor(t *testing.T) {
	var reviews int
	answer := func(allowedAfter int, created *bool) interceptor.Funcs {
		return interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch o := obj.(type) {
				case *authnv1.SelfSubjectReview:
					o.Status.UserInfo = authnv1.UserInfo{Username: "system:serviceaccount:quetzal:quetzal"}
					return nil
				case *authzv1.SelfSubjectAccessReview:
					reviews++
					ra := o.Spec.ResourceAttributes
					if ra == nil || ra.Namespace != "quetzal-srv-demo" || ra.Verb != "patch" || ra.Resource != "resourcequotas" {
						t.Errorf("asked about %+v, want patch resourcequotas in the new namespace", ra)
					}
					o.Status.Allowed = reviews > allowedAfter
					return nil
				case *rbacv1.RoleBinding:
					if !*created {
						*created = true
						return c.Create(ctx, obj, opts...)
					}
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "rolebindings"}, o.Name)
				}
				return c.Create(ctx, obj, opts...)
			},
		}
	}
	created := false
	cl := fake.NewClientBuilder().WithScheme(rbacScheme(t)).WithInterceptorFuncs(answer(2, &created)).Build()
	r := &Reconciler{Client: cl, NamespacedRole: "quetzal-namespaced", accessWait: 5 * time.Second}

	start := time.Now()
	r.ensureRoleBinding(context.Background(), "quetzal-srv-demo")
	if reviews != 3 {
		t.Errorf("asked %d times, want until the third answer allowed it", reviews)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s: it should stop as soon as the access is in effect", time.Since(start))
	}

	reviews = 0
	r.ensureRoleBinding(context.Background(), "quetzal-srv-demo")
	if reviews != 0 {
		t.Errorf("an existing binding was waited for (%d reviews)", reviews)
	}
}

// Access that never shows up is not waited for forever.
func TestRoleBindingWaitIsBounded(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(rbacScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch o := obj.(type) {
			case *authnv1.SelfSubjectReview:
				o.Status.UserInfo = authnv1.UserInfo{Username: "system:serviceaccount:quetzal:quetzal"}
				return nil
			case *authzv1.SelfSubjectAccessReview:
				return nil // never allowed
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := &Reconciler{Client: cl, NamespacedRole: "quetzal-namespaced", accessWait: 200 * time.Millisecond}
	start := time.Now()
	r.ensureRoleBinding(context.Background(), "quetzal-srv-demo")
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("waited %s, want about the 200ms bound", d)
	}
}
