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
	h2 := &models.DatabaseHost{ID: 9, Namespace: "custom-ns"}
	if host := ManagedDBServiceHost(h2); host != "quetzal-db.custom-ns.svc" {
		t.Errorf("custom-ns service host = %q", host)
	}
}

func TestBuildManagedDB(t *testing.T) {
	h := &models.DatabaseHost{ID: 3, Kind: models.DBHostManaged, Namespace: "quetzal-db-3", Image: "mariadb:11.4", StorageSize: "2Gi"}
	objs := buildManagedDB(h, "rootpw123")
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
