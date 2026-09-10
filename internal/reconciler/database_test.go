package reconciler

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
)

func TestManagedDBServiceHost(t *testing.T) {
	h := &models.DatabaseHost{ID: 5, Kind: models.DBHostManaged}
	if ns := ManagedDBNamespace(h); ns != "quetzal-db-5" {
		t.Errorf("namespace = %q", ns)
	}
	if host := ManagedDBServiceHost(h); host != "quetzal-db.quetzal-db-5.svc" {
		t.Errorf("service host = %q", host)
	}
	// A name of Quetzal's own shape is still the operator's to choose.
	h2 := &models.DatabaseHost{ID: 9, Namespace: "quetzal-db-analytics"}
	if host := ManagedDBServiceHost(h2); host != "quetzal-db.quetzal-db-analytics.svc" {
		t.Errorf("named namespace service host = %q", host)
	}
}

// Quetzal creates a managed host's namespace and deletes it when the host goes.
// Pointed at a namespace it did not create, it would put a workload in someone
// else's namespace and then collect it — and the namespace came from an API
// request, reachable by an admin scoped to database hosts alone. So a name it
// would not have chosen is ignored, and collection refuses it outright.
func TestManagedDBNamespaceCannotBeAimedElsewhere(t *testing.T) {
	for _, name := range []string{
		"kube-system", "default", "quetzal", "quetzal-srv-victim",
		"../kube-system", "Quetzal-DB-1", "", "quetzal-db",
	} {
		h := &models.DatabaseHost{ID: 7, Kind: models.DBHostManaged, Namespace: name}
		if got := ManagedDBNamespace(h); got != "quetzal-db-7" {
			t.Errorf("namespace %q was honoured as %q, want the derived quetzal-db-7", name, got)
		}
		if IsManagedDBNamespace(name) {
			t.Errorf("collection would delete %q", name)
		}
	}
	for _, name := range []string{"quetzal-db-7", "quetzal-db-analytics", "quetzal-db-a1"} {
		if !IsManagedDBNamespace(name) {
			t.Errorf("%q should be collectable: Quetzal could have chosen it", name)
		}
	}
	// Every object of a host with a hijacked namespace lands in the derived one.
	for _, o := range buildManagedDB(&models.DatabaseHost{
		ID: 7, Kind: models.DBHostManaged, Namespace: "kube-system",
	}, "pw", "inst") {
		if ns := o.GetNamespace(); ns != "" && ns != "quetzal-db-7" {
			t.Errorf("%T placed in %q", o, ns)
		}
		if o.GetName() == "kube-system" {
			t.Errorf("%T would be applied over kube-system itself", o)
		}
	}
}

func TestBuildManagedDB(t *testing.T) {
	h := &models.DatabaseHost{ID: 3, Kind: models.DBHostManaged, Namespace: "quetzal-db-3", Image: "mariadb:11.4", StorageSize: "2Gi"}
	objs := buildManagedDB(h, "rootpw123", "inst-test")
	if len(objs) != 5 {
		t.Fatalf("got %d objects, want 5 (ns, secret, pvc, deploy, svc)", len(objs))
	}
	var sawSecret, sawDeploy, sawSvc bool
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.Secret:
			sawSecret = true
			if string(v.Data[dbRootField]) != "rootpw123" {
				t.Errorf("root secret = %q", v.Data[dbRootField])
			}
		case *appsv1.Deployment:
			sawDeploy = true
			c := v.Spec.Template.Spec.Containers[0]
			if c.Image != "mariadb:11.4" {
				t.Errorf("image = %q", c.Image)
			}
			if len(c.Ports) == 0 || c.Ports[0].ContainerPort != ManagedDBPort {
				t.Errorf("port = %+v", c.Ports)
			}
			if c.Env[0].ValueFrom == nil || c.Env[0].ValueFrom.SecretKeyRef == nil {
				t.Errorf("root password should come from a secretKeyRef, got %+v", c.Env)
			}
			if v.Spec.Template.Spec.AutomountServiceAccountToken == nil || *v.Spec.Template.Spec.AutomountServiceAccountToken {
				t.Error("managed DB pod should not automount a SA token")
			}
		case *corev1.Service:
			sawSvc = true
			if v.Spec.Type != corev1.ServiceTypeClusterIP {
				t.Errorf("service type = %q, want ClusterIP", v.Spec.Type)
			}
		}
	}
	if !sawSecret || !sawDeploy || !sawSvc {
		t.Errorf("missing objects: secret=%v deploy=%v svc=%v", sawSecret, sawDeploy, sawSvc)
	}
}
