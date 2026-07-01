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
	dep := BuildDeployment(s, tmpl, "", nil)

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if !c.Stdin {
		t.Errorf("container.Stdin must be true for console attach")
	}
	// Startup runs via a bash-preferring wrapper (falls back to sh); the resolved
	// command is passed as $0 so bash-only egg syntax works.
	if len(c.Command) != 4 || c.Command[0] != "/bin/sh" || c.Command[1] != "-c" {
		t.Fatalf("command = %v", c.Command)
	}
	if !strings.Contains(c.Command[2], "bash -c") {
		t.Errorf("startup wrapper should prefer bash, got %q", c.Command[2])
	}
	if c.Command[3] != "echo ${MSG}; sleep 1" {
		t.Errorf("startup cmd = %q, want the substituted startup", c.Command[3])
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["MSG"] != "hi" {
		t.Errorf("env = %+v, want MSG=hi", c.Env)
	}
	// Wings-injected globals imported eggs assume (memory in MiB, primary port,
	// bind IP). Without SERVER_MEMORY, `-Xmx{{SERVER_MEMORY}}M` expands to `-Xmx M`.
	if env["SERVER_MEMORY"] != "1024" { // 1Gi
		t.Errorf("SERVER_MEMORY = %q, want 1024", env["SERVER_MEMORY"])
	}
	if env["SERVER_PORT"] != "25565" {
		t.Errorf("SERVER_PORT = %q, want 25565", env["SERVER_PORT"])
	}
	if env["SERVER_IP"] != "0.0.0.0" {
		t.Errorf("SERVER_IP = %q, want 0.0.0.0", env["SERVER_IP"])
	}
	// HOME must be the data dir (Pterodactyl parity) so $HOME-relative lookups
	// (e.g. SteamCMD's ~/.steam/sdk64/steamclient.so) resolve onto the volume.
	if env["HOME"] != "/data" {
		t.Errorf("HOME = %q, want /data", env["HOME"])
	}
	if env["TZ"] != "UTC" {
		t.Errorf("TZ = %q, want UTC", env["TZ"])
	}
	// STARTUP is the resolved invocation in shell form (Wings exports it for
	// entrypoints that `eval "$STARTUP"`).
	if env["STARTUP"] != "echo ${MSG}; sleep 1" {
		t.Errorf("STARTUP = %q, want the shell-form startup", env["STARTUP"])
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
	if ics := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers; len(ics) != 0 {
		t.Fatalf("expected no init containers, got %d", len(ics))
	}
	// With an install script -> a marker-guarded init container mounting the data
	// volume at the egg convention path.
	tmpl.Install = &models.InstallScript{Image: "debian:slim", Script: "echo installing > /mnt/server/world.txt"}
	ics := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers
	if len(ics) != 1 || ics[0].Name != "install" {
		t.Fatalf("expected one install init container, got %+v", ics)
	}
	ic := ics[0]
	if ic.Image != "debian:slim" {
		t.Errorf("install image = %q", ic.Image)
	}
	script := ic.Command[len(ic.Command)-1]
	for _, want := range []string{".quetzal-installed", "echo installing > /mnt/server/world.txt", "QUETZAL_INSTALL_GEN"} {
		if !strings.Contains(script, want) {
			t.Errorf("install script missing %q:\n%s", want, script)
		}
	}
	// The install generation is passed as env so a bump (reinstall) re-runs it.
	var hasGen bool
	for _, e := range ic.Env {
		if e.Name == "QUETZAL_INSTALL_GEN" {
			hasGen = true
		}
	}
	if !hasGen {
		t.Errorf("install container missing QUETZAL_INSTALL_GEN env: %+v", ic.Env)
	}
	if ic.VolumeMounts[0].MountPath != "/mnt/server" || ic.VolumeMounts[0].Name != "data" {
		t.Errorf("install mount = %+v, want data at /mnt/server", ic.VolumeMounts[0])
	}

	// The egg install script needs the server's variables in env (e.g.
	// ${SERVER_JARFILE}); without them an egg's installer downloads nothing.
	s.Env = map[string]string{"SERVER_JARFILE": "server.jar"}
	ic = BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers[0]
	var jar string
	for _, e := range ic.Env {
		if e.Name == "SERVER_JARFILE" {
			jar = e.Value
		}
	}
	if jar != "server.jar" {
		t.Errorf("install container missing the server's variables (SERVER_JARFILE), env=%+v", ic.Env)
	}
	// Install HOME is the install mount so installers writing to ~ (e.g. SteamCMD's
	// ~/.steam) land on the data volume.
	var home string
	for _, e := range ic.Env {
		if e.Name == "HOME" {
			home = e.Value
		}
	}
	if home != "/mnt/server" {
		t.Errorf("install HOME = %q, want /mnt/server", home)
	}
}

func TestInstallRunsAsRootAndChowns(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.Install = &models.InstallScript{Image: "debian:slim", Script: "apt-get update"}
	ic := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers[0]

	// Install must run as root (overriding the pod's non-root default) so installer
	// images can apt-get/apk and write root-owned paths.
	sc := ic.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Fatalf("install must run as root, got %+v", sc)
	}
	if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
		t.Error("install container must set runAsNonRoot=false to override the pod default")
	}
	// ...then hand the volume to the runtime user (988) so the non-root game pod can
	// read it (fsGroup is a no-op on local-path).
	script := ic.Command[len(ic.Command)-1]
	if !strings.Contains(script, "chown -R 988:988") {
		t.Errorf("install script should chown the data to the runtime uid:\n%s", script)
	}
}

func TestInstallChownsDeclaredUID(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	uid := int64(1000)
	tmpl.SecurityContext = models.SecurityContext{RunAsUser: &uid}
	tmpl.Install = &models.InstallScript{Image: "debian:slim", Script: "true"}
	ic := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers[0]
	script := ic.Command[len(ic.Command)-1]
	if !strings.Contains(script, "chown -R 1000:1000") {
		t.Errorf("install should chown to the template's declared uid:\n%s", script)
	}
}

func TestWingsEnvOmitsMemoryWhenUnlimited(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.Resources.Memory = "" // unlimited
	for _, e := range wingsEnv(s, tmpl) {
		if e.Name == "SERVER_MEMORY" {
			t.Errorf("SERVER_MEMORY should be omitted when no limit is set, got %q", e.Value)
		}
	}
}

func TestIsConsoleStop(t *testing.T) {
	if !isConsoleStop("stop") || !isConsoleStop("end") {
		t.Error("plain console commands should be sent to stdin")
	}
	if isConsoleStop("^C") || isConsoleStop("") {
		t.Error("signal tokens and empty commands must not be written to stdin")
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

	// Selects the restricted (untrusted) workload pods by the netpol label.
	if np.Spec.PodSelector.MatchLabels[netpolLabel] != netpolRestricted {
		t.Errorf("podSelector = %+v, want %s=%s", np.Spec.PodSelector, netpolLabel, netpolRestricted)
	}
	// The game pod and data-manager carry that label; the activator does not (it
	// needs apiserver egress the restrictive policy would block).
	if BuildDeployment(s, tmpl, "", nil).Spec.Template.Labels[netpolLabel] != netpolRestricted {
		t.Error("game pod should carry the restricted netpol label")
	}
	if BuildDataDeployment(s, tmpl, "", 1).Spec.Template.Labels[netpolLabel] != netpolRestricted {
		t.Error("data-manager pod should carry the restricted netpol label")
	}
	if _, ok := BuildActivatorDeployment(s, tmpl, ActivatorParams{Image: "x", WakeURL: "u"}).Spec.Template.Labels[netpolLabel]; ok {
		t.Error("activator must NOT carry the restricted netpol label (it needs apiserver egress)")
	}
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
	dep := BuildDeployment(s, tmpl, "", []string{"RCON_PASSWORD"})
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

func TestBuildNetworkPolicyAllowsSFTPPort(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.Ports = nil
	tmpl.Ports = nil // no game ports, so any ingress port is the SFTP one
	s.SFTP = models.SFTPConfig{Enabled: true}
	np := BuildNetworkPolicy(s, tmpl)
	var sawSFTP bool
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == SFTPPort {
				sawSFTP = true
			}
		}
	}
	if !sawSFTP {
		t.Errorf("expected an ingress rule allowing the SFTP port %d, got %+v", SFTPPort, np.Spec.Ingress)
	}
}

func TestEULARendering(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.ConfigFiles = nil // isolate: only the eula feature can trigger a render

	// No eula feature -> no eula spec, no config render.
	if len(eulaSpec(s, tmpl)) != 0 || needsConfigRender(s, tmpl) {
		t.Error("no eula feature -> nothing to render")
	}

	tmpl.Features = []string{"eula"}
	// Feature present but not accepted -> still nothing (server keeps asking).
	if len(eulaSpec(s, tmpl)) != 0 || needsConfigRender(s, tmpl) {
		t.Error("eula not accepted -> nothing rendered")
	}

	// Accepted -> render eula.txt=true and the render init runs (even with no
	// config.files) when a system image is configured.
	s.EULAAccepted = true
	spec := eulaSpec(s, tmpl)
	if len(spec) != 1 || spec[0].Path != "eula.txt" || spec[0].Find["eula"] != "true" {
		t.Fatalf("eulaSpec = %+v, want eula.txt eula=true", spec)
	}
	if !needsConfigRender(s, tmpl) {
		t.Error("accepted eula should require a config render")
	}
	var hasRender bool
	for _, ic := range BuildDeployment(s, tmpl, "quetzal:test", nil).Spec.Template.Spec.InitContainers {
		if ic.Name == "render-config" {
			hasRender = true
		}
	}
	if !hasRender {
		t.Error("accepted eula + system image should add the config-render init container")
	}
}

func TestPodSecurityContextDefaultsNonRoot(t *testing.T) {
	_, tmpl := testServerAndTemplate()
	// A template with a declared user keeps it (built-in templates).
	declared := int64(1000)
	tmpl.SecurityContext = models.SecurityContext{RunAsUser: &declared}
	if sc := buildPodSecurityContext(tmpl); sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("declared runAsUser should be kept, got %+v", sc.RunAsUser)
	}
	// An imported egg declares no securityContext -> default to non-root 988 with
	// fsGroup so the volume is writable (it must not run as root).
	tmpl.SecurityContext = models.SecurityContext{}
	sc := buildPodSecurityContext(tmpl)
	if sc.RunAsUser == nil || *sc.RunAsUser != defaultEggUID {
		t.Errorf("default runAsUser = %v, want %d", sc.RunAsUser, defaultEggUID)
	}
	if sc.FSGroup == nil || *sc.FSGroup != defaultEggUID {
		t.Errorf("default fsGroup = %v, want %d", sc.FSGroup, defaultEggUID)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("default must set runAsNonRoot")
	}
}

func TestBuildDeploymentWorkingDir(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.DataPath = "/data"
	c := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.Containers[0]
	// Egg startup commands use paths relative to the data dir (e.g. `-jar
	// server.jar`), so the game container must run there.
	if c.WorkingDir != "/data" {
		t.Errorf("game container WorkingDir = %q, want /data", c.WorkingDir)
	}
}

func TestBuildInstallScriptStripsCRLF(t *testing.T) {
	// A template stored with CRLF (imported before normalization) must still run:
	// the builder strips \r so POSIX shells don't choke on `then\r`.
	out := buildInstallScript("/mnt/server", "if [ -n \"$X\" ]; then\r\n echo hi\r\nfi\r\n")
	if strings.Contains(out, "\r") {
		t.Errorf("wrapped install script still contains CR: %q", out)
	}
}

func TestBuildActivatorRunsAsNumericNonroot(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	dep := BuildActivatorDeployment(s, tmpl, ActivatorParams{Image: "quetzal:test", WakeURL: "http://x/wake", Token: "tok"})
	sc := dep.Spec.Template.Spec.SecurityContext
	// The Quetzal distroless image has a non-numeric USER (nonroot); without a
	// numeric runAsUser the kubelet refuses the pod under runAsNonRoot.
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Fatalf("activator must set a numeric non-zero runAsUser, got %+v", sc)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("activator should run as non-root")
	}
}

func TestBuildDataDeploymentDropsSFTPWhenSuspended(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.SFTP = models.SFTPConfig{Enabled: true}
	s.DesiredState = models.StateSuspended
	dep := BuildDataDeployment(s, tmpl, "quetzal:test", 1)
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "sftp" {
			t.Error("a suspended server's data-manager must not run the sftp sidecar")
		}
	}
}

func TestBuildDeploymentDropsServiceAccountToken(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	dep := BuildDeployment(s, tmpl, "", nil)
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

func TestToShellTemplate(t *testing.T) {
	cases := map[string]string{
		"{{server.build.default.port}}":        "25565",
		"{{server.build.env.RCON_PASSWORD}}":   "${RCON_PASSWORD}",
		"{{ server.build.default.ip }}":        "0.0.0.0",
		"{{config.docker.interface}}":          "0.0.0.0", // Wings bridge IP -> bind-all in k8s
		"{{MOTD}}":                             "${MOTD}",
		"{{env.FOO}}":                          "${FOO}",
		"prefix-{{server.build.default.port}}": "prefix-25565",
		"{{unknown.thing}}":                    "{{unknown.thing}}", // left untouched
		"literal":                              "literal",
	}
	for in, want := range cases {
		if got := toShellTemplate(in, 25565); got != want {
			t.Errorf("toShellTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildDeploymentConfigFilesInitContainers(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.ConfigFiles = []models.ConfigFile{{
		Path:   "server.properties",
		Parser: "properties",
		Find: map[string]string{
			"server-port":   "{{server.build.default.port}}",
			"rcon.password": "{{server.build.env.RCON_PASSWORD}}",
			"motd":          "{{MOTD}}",
		},
	}}
	s.Env = map[string]string{"MOTD": "hi"}

	// Without a system image, no render init containers are added.
	if ics := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.InitContainers; len(ics) != 0 {
		t.Fatalf("expected no init containers without a system image, got %d", len(ics))
	}

	dep := BuildDeployment(s, tmpl, "quetzal:test", []string{"RCON_PASSWORD"})
	ics := dep.Spec.Template.Spec.InitContainers
	var copyC, renderC *corev1.Container
	for i := range ics {
		switch ics[i].Name {
		case "render-copy":
			copyC = &ics[i]
		case "render-config":
			renderC = &ics[i]
		}
	}
	if copyC == nil || renderC == nil {
		t.Fatalf("expected render-copy and render-config init containers, got %+v", ics)
	}
	if copyC.Image != "quetzal:test" {
		t.Errorf("copy image = %q, want the system image", copyC.Image)
	}
	if renderC.Image != s.Image {
		t.Errorf("render image = %q, want the game image %q (correct file ownership)", renderC.Image, s.Image)
	}

	// The spec carries shell templates, never the secret value.
	var blob string
	for _, e := range renderC.Env {
		if e.Name == "QUETZAL_CONFIG_FILES" {
			blob = e.Value
		}
	}
	if !strings.Contains(blob, `"server-port":"25565"`) {
		t.Errorf("port not substituted in spec: %s", blob)
	}
	if !strings.Contains(blob, `"rcon.password":"${RCON_PASSWORD}"`) {
		t.Errorf("secret should be a ${VAR} template, not a value: %s", blob)
	}
	if !strings.Contains(blob, `"motd":"${MOTD}"`) {
		t.Errorf("env ref not converted: %s", blob)
	}

	// The secret reaches the renderer via secretKeyRef (resolves ${RCON_PASSWORD}).
	var hasSecretRef bool
	for _, e := range renderC.Env {
		if e.Name == "RCON_PASSWORD" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			hasSecretRef = true
		}
	}
	if !hasSecretRef {
		t.Error("render-config should receive RCON_PASSWORD via secretKeyRef")
	}

	// The shared bin volume exists.
	var hasBinVol bool
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == renderBinVolume && v.EmptyDir != nil {
			hasBinVol = true
		}
	}
	if !hasBinVol {
		t.Error("expected the shared render-bin emptyDir volume")
	}
}

func TestBuildDeploymentGamePod(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	s.SFTP = models.SFTPConfig{Enabled: true}

	// SFTP lives in the data-manager pod now, so the game pod never carries it.
	dep := BuildDeployment(s, tmpl, "quetzal:test", nil)
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "sftp" {
			t.Error("game pod must not carry the sftp sidecar (it's in the data-manager pod)")
		}
	}

	// The game pod is co-located with the data-manager pod via podAffinity so they
	// can share the ReadWriteOnce data volume on one node.
	aff := dep.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil || len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("expected podAffinity to the data-manager pod")
	}
	term := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != "kubernetes.io/hostname" || term.LabelSelector.MatchLabels[DataLabel] != s.Slug {
		t.Errorf("podAffinity term = %+v, want same-host affinity to DataLabel=%s", term, s.Slug)
	}
}

func TestBuildDataDeployment(t *testing.T) {
	s, tmpl := testServerAndTemplate()

	dep := BuildDataDeployment(s, tmpl, "quetzal:test", 1)
	if dep.Name != DataDeployName || dep.Namespace != s.Namespace {
		t.Fatalf("name/ns = %s/%s", dep.Name, dep.Namespace)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", dep.Spec.Replicas)
	}
	pt := dep.Spec.Template
	// Must carry DataLabel and NOT ServerLabel (so the game Deployment never
	// adopts it and console/status code never mistakes it for the game container).
	if pt.Labels[DataLabel] != s.Slug {
		t.Errorf("data label = %q, want %q", pt.Labels[DataLabel], s.Slug)
	}
	if _, ok := pt.Labels[ServerLabel]; ok {
		t.Error("data-manager pod must not carry ServerLabel")
	}
	if dep.Spec.Selector.MatchLabels[DataLabel] != s.Slug {
		t.Errorf("selector = %v", dep.Spec.Selector.MatchLabels)
	}
	// Soft (preferred) co-location back to the game pod, so a data-manager-only
	// reschedule returns to the node holding the RWO volume (required would
	// deadlock against the game pod's required forward affinity).
	aff := pt.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil || len(aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("data-manager should prefer the game pod's node")
	}
	if aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm.LabelSelector.MatchLabels[ServerLabel] != s.Slug {
		t.Error("data-manager preferred affinity should target the game pod (ServerLabel)")
	}
	if len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("data-manager reverse affinity must be preferred, not required (deadlock)")
	}
	if pt.Spec.AutomountServiceAccountToken == nil || *pt.Spec.AutomountServiceAccountToken {
		t.Error("service account token must not be automounted")
	}
	if len(pt.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1 (no sftp without it enabled)", len(pt.Spec.Containers))
	}
	c := pt.Spec.Containers[0]
	// Container name must match the workload so console.Exec targets it.
	if c.Name != WorkloadName {
		t.Errorf("container name = %q, want %q", c.Name, WorkloadName)
	}
	if c.Image != s.Image {
		t.Errorf("image = %q, want game image %q", c.Image, s.Image)
	}
	if len(c.Command) == 0 || c.Command[0] != "sleep" {
		t.Errorf("command = %v, want sleep keepalive", c.Command)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != dataVolume || c.VolumeMounts[0].MountPath != tmpl.DataPath {
		t.Errorf("volume mount = %+v, want data at %s", c.VolumeMounts, tmpl.DataPath)
	}

	// The reconciler passes 0 replicas during a restore (exclusive volume access).
	if zero := BuildDataDeployment(s, tmpl, "quetzal:test", 0); zero.Spec.Replicas == nil || *zero.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want 0", zero.Spec.Replicas)
	}

	// With SFTP enabled + a system image: the sftp sidecar + copy init + volumes
	// live here (not in the game pod).
	s.SFTP = models.SFTPConfig{Enabled: true}
	sftpDep := BuildDataDeployment(s, tmpl, "quetzal:test", 1)
	var sidecar, copyInit bool
	for _, c := range sftpDep.Spec.Template.Spec.Containers {
		if c.Name == "sftp" {
			sidecar = true
			if c.Image != s.Image {
				t.Errorf("sftp image = %q, want game image %q", c.Image, s.Image)
			}
		}
	}
	for _, c := range sftpDep.Spec.Template.Spec.InitContainers {
		if c.Name == "sftp-copy" {
			copyInit = true
		}
	}
	if !sidecar || !copyInit {
		t.Fatalf("expected sftp sidecar (%v) and sftp-copy init (%v)", sidecar, copyInit)
	}
	vols := map[string]bool{}
	for _, v := range sftpDep.Spec.Template.Spec.Volumes {
		vols[v.Name] = true
	}
	for _, want := range []string{sftpHostKeyVol, sftpAuthKeyVol, renderBinVolume} {
		if !vols[want] {
			t.Errorf("missing volume %q", want)
		}
	}

	// Without a system image, no sftp sidecar even when enabled.
	for _, c := range BuildDataDeployment(s, tmpl, "", 1).Spec.Template.Spec.Containers {
		if c.Name == "sftp" {
			t.Error("no sftp sidecar expected without a system image")
		}
	}
}

func TestBuildSFTPServiceAndAuthKeys(t *testing.T) {
	s, _ := testServerAndTemplate()
	svc := BuildSFTPService(s, 31234)
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("sftp service type = %v, want NodePort", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != SFTPPort {
		t.Errorf("sftp service ports = %+v", svc.Spec.Ports)
	}
	// The NodePort is the one allocated from Quetzal's pool (not k8s auto-assign).
	if svc.Spec.Ports[0].NodePort != 31234 {
		t.Errorf("sftp nodePort = %d, want 31234 (from the pool)", svc.Spec.Ports[0].NodePort)
	}
	// 0 leaves it to Kubernetes (fallback).
	if BuildSFTPService(s, 0).Spec.Ports[0].NodePort != 0 {
		t.Error("nodePort 0 should leave the field unset")
	}
	// SFTP runs in the data-manager pod, so the Service selects it by DataLabel.
	if svc.Spec.Selector[DataLabel] != s.Slug {
		t.Errorf("sftp service selector = %v, want DataLabel=%s", svc.Spec.Selector, s.Slug)
	}
	cm := BuildSFTPAuthKeysConfigMap(s, []string{"ssh-ed25519 AAAA alice", "ssh-ed25519 BBBB bob"})
	got := cm.Data[SFTPAuthKeysField]
	if !strings.Contains(got, "alice") || !strings.Contains(got, "bob") {
		t.Errorf("authorized_keys = %q", got)
	}
}
