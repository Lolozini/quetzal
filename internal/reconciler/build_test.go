package reconciler

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func testServerAndTemplate() (*models.Server, *models.Template) {
	t := &models.Template{
		Slug:     "demo",
		Name:     "Demo",
		Startup:  "echo {{MSG}}; sleep 1",
		DataPath: "/data",
		Console:  models.ConsoleConfig{Type: models.ConsoleAttach},
		Ports:    []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true}},
	}
	s := &models.Server{
		Slug:         "s1",
		Image:        "alpine:3.20",
		Namespace:    "quetzal-srv-s1",
		DesiredState: models.StateRunning,
		Resources:    models.Resources{Memory: "1Gi", CPU: "1"},
		Env:          map[string]string{"MSG": "hi"},
		Storage:      models.Storage{Type: models.StoragePVC, Size: "5Gi"},
		Ports:        t.Ports,
	}
	return s, t
}

func TestBuildDeployment(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	dep := BuildDeployment(s, tmpl)

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if !c.Stdin {
		t.Errorf("container.Stdin must be true for console attach")
	}
	wantCmd := []string{"/bin/sh", "-c", "echo ${MSG}; sleep 1"}
	if len(c.Command) != len(wantCmd) {
		t.Fatalf("command = %v", c.Command)
	}
	for i := range wantCmd {
		if c.Command[i] != wantCmd[i] {
			t.Errorf("command[%d] = %q, want %q", i, c.Command[i], wantCmd[i])
		}
	}
	if len(c.Env) != 1 || c.Env[0].Name != "MSG" || c.Env[0].Value != "hi" {
		t.Errorf("env = %+v", c.Env)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("volumeMounts = %+v", c.VolumeMounts)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation should be false")
	}

	// Stopped -> 0 replicas
	s.DesiredState = models.StateStopped
	if r := s.Replicas(); r != 0 {
		t.Errorf("stopped replicas = %d, want 0", r)
	}
}

func TestBuildPVCAndService(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	pvc := BuildPVC(s)
	if pvc == nil {
		t.Fatal("expected PVC for pvc storage")
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "5Gi" {
		t.Errorf("pvc size = %s, want 5Gi", got)
	}

	// hostPath storage -> no PVC
	s.Storage = models.Storage{Type: models.StorageHostPath, HostPath: "/srv/x"}
	if BuildPVC(s) != nil {
		t.Error("expected no PVC for hostPath storage")
	}

	svc := BuildService(s, tmpl)
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}
}

func TestBuildNetworkPolicyBlocksMetadata(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	np := BuildNetworkPolicy(s, tmpl)

	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("ingress = %+v", np.Spec.Ingress)
	}
	// Last egress rule should allow internet except the metadata IP.
	found := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				for _, ex := range peer.IPBlock.Except {
					if ex == metadataIP {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("egress should exclude node metadata IP %s", metadataIP)
	}
}

func TestBuildNetworkPolicyPortlessDeniesIngress(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.Ports = nil
	tmpl.Ports = nil
	np := BuildNetworkPolicy(s, tmpl)
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("portless server should have no ingress rules (deny-all), got %+v", np.Spec.Ingress)
	}
}
