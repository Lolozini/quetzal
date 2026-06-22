package reconciler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
	dep := BuildDeployment(s, tmpl, nil)

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

func TestBuildDeploymentInstallInitContainer(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	// No install -> no init container.
	if ics := BuildDeployment(s, tmpl, nil).Spec.Template.Spec.InitContainers; len(ics) != 0 {
		t.Fatalf("expected no init containers, got %d", len(ics))
	}
	// With an install script -> a marker-guarded init container mounting the data
	// volume at the egg convention path.
	tmpl.Install = &models.InstallScript{Image: "debian:slim", Script: "echo installing > /mnt/server/world.txt"}
	ics := BuildDeployment(s, tmpl, nil).Spec.Template.Spec.InitContainers
	if len(ics) != 1 || ics[0].Name != "install" {
		t.Fatalf("expected one install init container, got %+v", ics)
	}
	ic := ics[0]
	if ic.Image != "debian:slim" {
		t.Errorf("install image = %q", ic.Image)
	}
	script := ic.Command[len(ic.Command)-1]
	for _, want := range []string{".quetzal-installed", "echo installing > /mnt/server/world.txt", "touch"} {
		if !strings.Contains(script, want) {
			t.Errorf("install script missing %q:\n%s", want, script)
		}
	}
	if ic.VolumeMounts[0].MountPath != "/mnt/server" || ic.VolumeMounts[0].Name != "data" {
		t.Errorf("install mount = %+v, want data at /mnt/server", ic.VolumeMounts[0])
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

	svc := BuildService(s, tmpl, false)
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}
}

func TestBuildServiceClusterIPDefault(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	svc := BuildService(s, tmpl, false)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("default type = %q, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("ClusterIP must not set externalTrafficPolicy, got %q", svc.Spec.ExternalTrafficPolicy)
	}
}

func TestBuildServiceNodePort(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.Expose = models.Expose{Type: models.ExposeNodePort}
	s.Ports = []models.PortSpec{{Name: "game", Port: 25565, Protocol: "TCP", Primary: true, NodePort: 30123}}

	svc := BuildService(s, tmpl, false)
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("type = %q, want NodePort", svc.Spec.Type)
	}
	if svc.Spec.Ports[0].NodePort != 30123 {
		t.Errorf("nodePort = %d, want 30123", svc.Spec.Ports[0].NodePort)
	}
	// Defaults to preserving the client source IP for external exposure.
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", svc.Spec.ExternalTrafficPolicy)
	}
}

func TestBuildServiceLoadBalancerAnnotationsAndOptOut(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	cluster := false
	s.Expose = models.Expose{
		Type:             models.ExposeLoadBalancer,
		Annotations:      map[string]string{"external-dns.alpha.kubernetes.io/hostname": "mc.example.com"},
		PreserveClientIP: &cluster,
	}
	svc := BuildService(s, tmpl, false)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("type = %q, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Annotations["external-dns.alpha.kubernetes.io/hostname"] != "mc.example.com" {
		t.Errorf("annotations not propagated: %+v", svc.Annotations)
	}
	// PreserveClientIP=false opts out of externalTrafficPolicy: Local.
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want unset (opted out)", svc.Spec.ExternalTrafficPolicy)
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

func TestBuildDeploymentSecretEnv(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.Env = map[string]string{"PUBLIC": "ok"}
	dep := BuildDeployment(s, tmpl, []string{"RCON_PASSWORD"})
	c := dep.Spec.Template.Spec.Containers[0]

	var public, secret *struct{}
	for _, e := range c.Env {
		switch e.Name {
		case "PUBLIC":
			if e.Value != "ok" || e.ValueFrom != nil {
				t.Errorf("PUBLIC should be a plain value, got %+v", e)
			}
			public = &struct{}{}
		case "RCON_PASSWORD":
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("RCON_PASSWORD should use secretKeyRef, got %+v", e)
			} else if e.ValueFrom.SecretKeyRef.Name != "server-env" {
				t.Errorf("secret name = %q, want server-env", e.ValueFrom.SecretKeyRef.Name)
			}
			secret = &struct{}{}
		}
	}
	if public == nil || secret == nil {
		t.Fatalf("missing env entries: %+v", c.Env)
	}
}

func TestBuildSecret(t *testing.T) {
	s, _ := testServerAndTemplate()
	if BuildSecret(s, nil) != nil {
		t.Error("no data -> nil secret")
	}
	sec := BuildSecret(s, map[string]string{"RCON_PASSWORD": "x"})
	if sec == nil || sec.Name != "server-env" || sec.StringData["RCON_PASSWORD"] != "x" {
		t.Errorf("unexpected secret: %+v", sec)
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

func TestBuildDeploymentDropsServiceAccountToken(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	dep := BuildDeployment(s, tmpl, nil)
	got := dep.Spec.Template.Spec.AutomountServiceAccountToken
	if got == nil || *got {
		t.Errorf("AutomountServiceAccountToken = %v, want false (untrusted game code)", got)
	}
}

func TestBuildResourceQuotaCapsCountsNotCompute(t *testing.T) {
	s, _ := testServerAndTemplate()
	q := BuildResourceQuota(s)
	if q.Namespace != s.Namespace {
		t.Errorf("quota namespace = %q, want %q", q.Namespace, s.Namespace)
	}
	hard := q.Spec.Hard
	if _, ok := hard[corev1.ResourcePods]; !ok {
		t.Error("quota should cap pod count")
	}
	// Must NOT cap total CPU/memory: backup/restore Jobs share the namespace and
	// a tight compute quota would also force every pod to declare limits.
	for _, r := range []corev1.ResourceName{corev1.ResourceLimitsCPU, corev1.ResourceLimitsMemory, corev1.ResourceRequestsCPU, corev1.ResourceRequestsMemory} {
		if _, ok := hard[r]; ok {
			t.Errorf("quota must not cap compute resource %q", r)
		}
	}
}
