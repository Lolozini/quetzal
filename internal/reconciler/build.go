package reconciler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/lolozini/quetzal/internal/configfile"
	"github.com/lolozini/quetzal/internal/models"
)

const (
	// configRenderBinary is where the renderer lives in the Quetzal image; the
	// copy init container installs it into renderBinPath on a shared volume so the
	// render init (running the game image, hence the server's user) can execute it.
	configRenderBinary = "/usr/local/bin/quetzal-configrender"
	renderBinVolume    = "quetzal-config-bin"
	renderBinMount     = "/quetzal-bin"
	renderBinPath      = "/quetzal-bin/configrender"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "quetzal"
	// ServerLabel marks objects belonging to a given server (value = slug).
	ServerLabel = "quetzal.dev/server"
	// ActivatorLabel marks a server's wake-on-connect activator pods (value =
	// slug). It is deliberately distinct from ServerLabel so the real workload's
	// Deployment never adopts activator pods; the Service selector flips between
	// the two.
	ActivatorLabel = "quetzal.dev/activator"
	// ActivatorName is the activator Deployment's name within a server namespace.
	ActivatorName = "activator"
	// MaintLabel marks a server's ephemeral maintenance pod (value = slug). Like
	// ActivatorLabel it is deliberately distinct from ServerLabel so the real
	// workload's Deployment never adopts (and then scales away) the maintenance
	// pod, and so console/status code never mistakes it for the game container.
	MaintLabel = "quetzal.dev/maint"
	// MaintName is the maintenance pod's name within a server namespace.
	MaintName = "maintenance"
	// WorkloadName is the Deployment/Service name within a server's namespace.
	WorkloadName = "server"
	// DataVolume is the name of a server's data PVC/volume.
	DataVolume    = "data"
	serverLabel   = ServerLabel
	workloadName  = WorkloadName
	dataVolume    = DataVolume
	envSecretName = "server-env" // per-server Secret holding sensitive env
	metadataIP    = "169.254.169.254/32"
)

// labelsFor returns the standard labels for a server's objects.
func labelsFor(s *models.Server) map[string]string {
	return map[string]string{
		managedByLabel: managedByValue,
		serverLabel:    s.Slug,
	}
}

// BuildNamespace returns the per-server namespace.
func BuildNamespace(s *models.Server) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   s.Namespace,
			Labels: labelsFor(s),
		},
	}
}

// BuildPVC returns the data PersistentVolumeClaim backing the server.
func BuildPVC(s *models.Server) *corev1.PersistentVolumeClaim {
	size := s.Storage.Size
	if size == "" {
		size = "10Gi"
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataVolume,
			Namespace: s.Namespace,
			Labels:    labelsFor(s),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
	if s.Storage.StorageClass != "" {
		sc := s.Storage.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc
}

// BuildSecret returns the per-server Secret holding sensitive env values, or
// nil when there are none.
func BuildSecret(s *models.Server, data map[string]string) *corev1.Secret {
	if len(data) == 0 {
		return nil
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envSecretName,
			Namespace: s.Namespace,
			Labels:    labelsFor(s),
		},
		StringData: data,
	}
}

// BuildDeployment projects a server (+ template) into a Deployment. secretKeys
// lists env var names sourced from the per-server Secret (via secretKeyRef).
func BuildDeployment(s *models.Server, t *models.Template, systemImage string, secretKeys []string) *appsv1.Deployment {
	labels := labelsFor(s)
	replicas := s.Replicas()

	dataPath := t.DataPath
	if dataPath == "" {
		dataPath = "/data"
	}

	container := corev1.Container{
		Name:            workloadName,
		Image:           s.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		// Interim startup shim: when a template defines a startup command, run it
		// via a shell with {{VAR}} -> ${VAR} substitution. The full shim (config
		// file rendering + sanitization, per the plan) lands in a later phase.
		// When empty (e.g. itzg images), the image entrypoint is used as-is.
		Command: startupCommand(t),
		// stdin must stay open so the console can attach to send commands.
		Stdin:     true,
		TTY:       false,
		Env:       buildEnv(s.Env, secretKeys),
		Ports:     buildContainerPorts(serverPorts(s, t)),
		Resources: buildResources(s.Resources),
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolume, MountPath: dataPath},
		},
		SecurityContext: buildContainerSecurityContext(t),
	}

	grace := int64(30)
	if t.StopGraceSeconds > 0 {
		grace = int64(t.StopGraceSeconds)
	}

	noAutomount := false
	initContainers := installInitContainers(s, t)
	containers := []corev1.Container{container}
	volumes := []corev1.Volume{buildDataVolume(s)}

	// Helper binaries (config render, sftp) are copied out of the Quetzal image
	// into a shared volume by copy init containers, then executed from the game
	// image so they run as the server's own user (correct file ownership).
	sftp := systemImage != "" && s.SFTP.Enabled
	needBin := systemImage != "" && (len(t.ConfigFiles) > 0 || sftp)

	if systemImage != "" && len(t.ConfigFiles) > 0 {
		initContainers = append(initContainers, configRenderInitContainers(s, t, systemImage, secretKeys, dataPath)...)
	}
	if sftp {
		initContainers = append(initContainers, sftpCopyInitContainer(systemImage, t))
		containers = append(containers, sftpSidecar(s, t, dataPath))
		volumes = append(volumes, sftpVolumes()...)
	}
	if needBin {
		volumes = append(volumes, corev1.Volume{
			Name:         renderBinVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	pod := corev1.PodSpec{
		Containers:     containers,
		InitContainers: initContainers,
		// Untrusted game code (mods/plugins) has no business talking to the
		// Kubernetes API, so don't mount a ServiceAccount token into the pod.
		AutomountServiceAccountToken:  &noAutomount,
		SecurityContext:               buildPodSecurityContext(t),
		Volumes:                       volumes,
		NodeSelector:                  s.NodeSelector,
		TerminationGracePeriodSeconds: &grace,
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Game servers are stateful: never run two at once on one volume.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{serverLabel: s.Slug}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       pod,
			},
		},
	}
}

// BuildMaintenancePod builds an ephemeral pod that mounts a server's data volume
// so files can be managed while the server itself is stopped (its Deployment is
// scaled to zero, freeing the ReadWriteOnce volume). It runs the game image, so
// it has the same tools and runs as the server's own user (preserving file
// ownership), but only sleeps; file operations exec into it the same way they do
// the live container (its container is named WorkloadName so console.Exec targets
// it). It carries MaintLabel (not ServerLabel) so the Deployment never adopts it,
// and self-terminates after ttlSeconds (activeDeadlineSeconds) as a backstop; the
// reconciler also removes it whenever the server is meant to be running, so it
// never blocks the data volume against the real workload.
func BuildMaintenancePod(s *models.Server, t *models.Template, ttlSeconds int64) *corev1.Pod {
	dataPath := t.DataPath
	if dataPath == "" {
		dataPath = "/data"
	}
	no := false
	deadline := ttlSeconds
	// `sleep` runs as PID 1 and ignores SIGTERM, so keep the grace period short:
	// the pod holds no state to flush, and it must release the ReadWriteOnce data
	// volume promptly when the server starts (the reconciler also force-deletes it).
	grace := int64(2)
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      MaintName,
			Namespace: s.Namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				MaintLabel:     s.Slug,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  &no,
			SecurityContext:               buildPodSecurityContext(t),
			NodeSelector:                  s.NodeSelector,
			Volumes:                       []corev1.Volume{buildDataVolume(s)},
			Containers: []corev1.Container{{
				Name:            workloadName,
				Image:           s.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Idle keepalive; activeDeadlineSeconds bounds the real lifetime.
				Command:      []string{"sleep", strconv.FormatInt(ttlSeconds+3600, 10)},
				VolumeMounts: []corev1.VolumeMount{{Name: dataVolume, MountPath: dataPath}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
				SecurityContext: buildContainerSecurityContext(t),
			}},
		},
	}
}

// BuildService projects a server into a Service whose type follows the server's
// exposure (ClusterIP by default, NodePort or LoadBalancer when published). When
// activator is true the selector points at the wake-on-connect activator pods
// instead of the (scaled-to-zero) real workload.
func BuildService(s *models.Server, t *models.Template, activator bool) *corev1.Service {
	selectorLabel, selectorValue := serverLabel, s.Slug
	if activator {
		selectorLabel = ActivatorLabel
	}
	exposeNodePort := s.Expose.ServiceType() == models.ExposeNodePort
	var ports []corev1.ServicePort
	for _, p := range serverPorts(s, t) {
		sp := corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   protocol(p.Protocol),
		}
		// Honour the pool-allocated node port so it stays stable; 0 lets
		// Kubernetes pick one.
		if exposeNodePort && p.NodePort > 0 {
			sp.NodePort = p.NodePort
		}
		ports = append(ports, sp)
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        workloadName,
			Namespace:   s.Namespace,
			Labels:      labelsFor(s),
			Annotations: s.Expose.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     serviceType(s.Expose.ServiceType()),
			Selector: map[string]string{selectorLabel: selectorValue},
			Ports:    ports,
		},
	}
	// Preserve the client source IP for published game traffic (bans/geo).
	// Invalid for ClusterIP, so only set it when externally exposed.
	if s.Expose.External() && s.Expose.LocalTraffic() {
		svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	}
	return svc
}

// activatorBinary is the activator command inside the Quetzal image.
const activatorBinary = "/usr/local/bin/quetzal-activator"

// InternalServiceName is the per-server ClusterIP Service that always selects the
// real workload, giving the proxy activator a stable backend address.
const InternalServiceName = "server-internal"

// ActivatorParams configures a server's activator Deployment.
type ActivatorParams struct {
	Image     string
	WakeURL   string
	ActiveURL string // proxy mode only
	Token     string
	Proxy     bool // true = always-in-path TCP+UDP proxy; false = lightweight wake-and-drop
}

// BuildActivatorDeployment renders a server's activator. In drop mode it listens
// on the TCP ports and wakes on connect; in proxy mode it forwards TCP+UDP to the
// internal Service and reports activity.
func BuildActivatorDeployment(s *models.Server, t *models.Template, p ActivatorParams) *appsv1.Deployment {
	labels := map[string]string{managedByLabel: managedByValue, ActivatorLabel: s.Slug}
	one := int32(1)
	no := false
	yes := true

	ports := serverPorts(s, t)
	var tcpCSV, udpCSV []string
	var cports []corev1.ContainerPort
	for _, pt := range tcpPorts(ports) {
		tcpCSV = append(tcpCSV, strconv.Itoa(int(pt.Port)))
		cports = append(cports, corev1.ContainerPort{ContainerPort: pt.Port, Protocol: corev1.ProtocolTCP})
	}
	env := []corev1.EnvVar{
		{Name: "QUETZAL_WAKE_URL", Value: p.WakeURL},
		{Name: "QUETZAL_WAKE_SLUG", Value: s.Slug},
		{Name: "QUETZAL_WAKE_TOKEN", Value: p.Token},
		{Name: "QUETZAL_TCP_PORTS", Value: strings.Join(tcpCSV, ",")},
	}
	if p.Proxy {
		for _, pt := range udpPorts(ports) {
			udpCSV = append(udpCSV, strconv.Itoa(int(pt.Port)))
			cports = append(cports, corev1.ContainerPort{ContainerPort: pt.Port, Protocol: corev1.ProtocolUDP})
		}
		env = append(env,
			corev1.EnvVar{Name: "QUETZAL_MODE", Value: "proxy"},
			corev1.EnvVar{Name: "QUETZAL_BACKEND", Value: fmt.Sprintf("%s.%s.svc", InternalServiceName, s.Namespace)},
			corev1.EnvVar{Name: "QUETZAL_UDP_PORTS", Value: strings.Join(udpCSV, ",")},
			corev1.EnvVar{Name: "QUETZAL_ACTIVE_URL", Value: p.ActiveURL},
		)
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ActivatorName, Namespace: s.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{ActivatorLabel: s.Slug}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// The activator authenticates to the apiserver with an HMAC
					// token over HTTP; it needs no Kubernetes API access.
					AutomountServiceAccountToken: &no,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "activator",
						Image:           p.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{activatorBinary},
						Ports:           cports,
						Env:             env,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// BuildInternalService is the proxy's stable backend: a ClusterIP Service that
// always selects the real workload (so the proxy can reach it by a fixed DNS
// name regardless of the public Service's selector).
func BuildInternalService(s *models.Server, t *models.Template) *corev1.Service {
	var ports []corev1.ServicePort
	for _, p := range serverPorts(s, t) {
		ports = append(ports, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   protocol(p.Protocol),
		})
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      InternalServiceName,
			Namespace: s.Namespace,
			Labels:    labelsFor(s),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{serverLabel: s.Slug},
			Ports:    ports,
		},
	}
}

// tcpPorts / udpPorts split a port list by protocol (empty protocol = TCP).
func tcpPorts(ports []models.PortSpec) []models.PortSpec {
	var out []models.PortSpec
	for _, p := range ports {
		if !strings.EqualFold(p.Protocol, "UDP") {
			out = append(out, p)
		}
	}
	return out
}

func udpPorts(ports []models.PortSpec) []models.PortSpec {
	var out []models.PortSpec
	for _, p := range ports {
		if strings.EqualFold(p.Protocol, "UDP") {
			out = append(out, p)
		}
	}
	return out
}

func hasTCPPort(ports []models.PortSpec) bool { return len(tcpPorts(ports)) > 0 }

func serviceType(e models.ExposeType) corev1.ServiceType {
	switch e {
	case models.ExposeNodePort:
		return corev1.ServiceTypeNodePort
	case models.ExposeLoadBalancer:
		return corev1.ServiceTypeLoadBalancer
	default:
		return corev1.ServiceTypeClusterIP
	}
}

// BuildNetworkPolicy returns a secure-by-default policy: ingress only to the
// declared game ports; egress to DNS and the internet but NOT the node metadata
// endpoint. NOTE: blocking the in-cluster API server and pod/service CIDRs is
// handled in a later phase (needs cluster-specific config).
func BuildNetworkPolicy(s *models.Server, t *models.Template) *networkingv1.NetworkPolicy {
	var ingressPorts []networkingv1.NetworkPolicyPort
	for _, p := range serverPorts(s, t) {
		proto := protocol(p.Protocol)
		port := intstr.FromInt32(p.Port)
		ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{
			Protocol: &proto,
			Port:     &port,
		})
	}

	dnsUDP := corev1.ProtocolUDP
	dnsTCP := corev1.ProtocolTCP
	dnsPort := intstr.FromInt32(53)

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quetzal-default",
			Namespace: s.Namespace,
			Labels:    labelsFor(s),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{serverLabel: s.Slug}},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Ingress only on declared game ports; when a server exposes no
			// ports, leaving this empty denies all ingress (secure default).
			Ingress: ingressRules(ingressPorts),
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{ // DNS
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &dnsUDP, Port: &dnsPort},
						{Protocol: &dnsTCP, Port: &dnsPort},
					},
				},
				{ // internet, minus node metadata
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{
							CIDR:   "0.0.0.0/0",
							Except: []string{metadataIP},
						},
					}},
				},
			},
		},
	}
}

// BuildResourceQuota caps how many objects a server's namespace may hold, to
// bound the blast radius of a compromised workload (it can't spawn many pods or
// claim many volumes). It deliberately does NOT cap total CPU/memory: backup and
// restore run as Jobs in the same namespace, so a tight compute quota would
// break them, and per-pod limits plus per-user quotas already bound compute. The
// pod count uses non-terminal pods, so completed backup Job pods don't count.
func BuildResourceQuota(s *models.Server) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quetzal-quota",
			Namespace: s.Namespace,
			Labels:    labelsFor(s),
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				// Recreate strategy => at most one server pod; +1 activator
				// (wake-on-connect) +1 transient backup/restore Job pod, with headroom.
				corev1.ResourcePods:                   resource.MustParse("6"),
				corev1.ResourceServices:               resource.MustParse("4"),
				corev1.ResourcePersistentVolumeClaims: resource.MustParse("3"),
			},
		},
	}
}

// ---- helpers ----

func ingressRules(ports []networkingv1.NetworkPolicyPort) []networkingv1.NetworkPolicyIngressRule {
	if len(ports) == 0 {
		return nil
	}
	return []networkingv1.NetworkPolicyIngressRule{{Ports: ports}}
}

func serverPorts(s *models.Server, t *models.Template) []models.PortSpec {
	if len(s.Ports) > 0 {
		return s.Ports
	}
	return t.Ports
}

func buildEnv(env map[string]string, secretKeys []string) []corev1.EnvVar {
	secretSet := make(map[string]bool, len(secretKeys))
	for _, k := range secretKeys {
		secretSet[k] = true
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		if !secretSet[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // deterministic ordering
	out := make([]corev1.EnvVar, 0, len(keys)+len(secretKeys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}

	sk := append([]string(nil), secretKeys...)
	sort.Strings(sk)
	for _, k := range sk {
		out = append(out, corev1.EnvVar{
			Name: k,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: envSecretName},
					Key:                  k,
				},
			},
		})
	}
	return out
}

func buildContainerPorts(ports []models.PortSpec) []corev1.ContainerPort {
	var out []corev1.ContainerPort
	for _, p := range ports {
		out = append(out, corev1.ContainerPort{
			Name:          p.Name,
			ContainerPort: p.Port,
			Protocol:      protocol(p.Protocol),
		})
	}
	return out
}

func buildResources(r models.Resources) corev1.ResourceRequirements {
	limits := corev1.ResourceList{}
	if r.Memory != "" {
		limits[corev1.ResourceMemory] = resource.MustParse(r.Memory)
	}
	if r.CPU != "" {
		limits[corev1.ResourceCPU] = resource.MustParse(r.CPU)
	}
	if len(limits) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: limits}
}

func buildDataVolume(s *models.Server) corev1.Volume {
	return corev1.Volume{
		Name: dataVolume,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: dataVolume,
			},
		},
	}
}

func buildPodSecurityContext(t *models.Template) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if t.SecurityContext.FSGroup != nil {
		sc.FSGroup = t.SecurityContext.FSGroup
	}
	if t.SecurityContext.RunAsUser != nil {
		sc.RunAsUser = t.SecurityContext.RunAsUser
	}
	if t.SecurityContext.RunAsGroup != nil {
		sc.RunAsGroup = t.SecurityContext.RunAsGroup
	}
	if t.SecurityContext.RunAsNonRoot != nil {
		sc.RunAsNonRoot = t.SecurityContext.RunAsNonRoot
	}
	return sc
}

func buildContainerSecurityContext(t *models.Template) *corev1.SecurityContext {
	no := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// installMountPath is where the data volume is mounted during install, matching
// the Pterodactyl egg convention so imported install scripts work unchanged.
const installMountPath = "/mnt/server"

// installInitContainers runs a template's install script once (egg
// scripts.installation), populating the data volume before the main container
// starts. A marker file makes it a no-op on every subsequent start.
// configRenderInitContainers returns the copy + render init containers that
// apply a template's config.files. The render step uses the game image (s.Image)
// and the same env as the main container, so ${VAR} (including secret env via
// secretKeyRef) resolves and files are written as the server's user.
func configRenderInitContainers(s *models.Server, t *models.Template, systemImage string, secretKeys []string, dataPath string) []corev1.Container {
	primary := primaryPort(s, t)
	specs := make([]configfile.Spec, 0, len(t.ConfigFiles))
	for _, cf := range t.ConfigFiles {
		find := make(map[string]string, len(cf.Find))
		for k, v := range cf.Find {
			find[k] = toShellTemplate(v, primary)
		}
		specs = append(specs, configfile.Spec{Path: cf.Path, Parser: string(cf.Parser), Find: find})
	}
	blob, _ := json.Marshal(specs)

	renderEnv := append(buildEnv(s.Env, secretKeys),
		corev1.EnvVar{Name: "QUETZAL_DATA_PATH", Value: dataPath},
		corev1.EnvVar{Name: "QUETZAL_CONFIG_FILES", Value: string(blob)},
	)
	sc := buildContainerSecurityContext(t)
	return []corev1.Container{
		{
			Name:            "render-copy",
			Image:           systemImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{configRenderBinary},
			Env:             []corev1.EnvVar{{Name: "QUETZAL_INSTALL_TO", Value: renderBinPath}},
			VolumeMounts:    []corev1.VolumeMount{{Name: renderBinVolume, MountPath: renderBinMount}},
			SecurityContext: sc,
		},
		{
			Name:            "render-config",
			Image:           s.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{renderBinPath},
			Env:             renderEnv,
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataVolume, MountPath: dataPath},
				{Name: renderBinVolume, MountPath: renderBinMount, ReadOnly: true},
			},
			SecurityContext: sc,
		},
	}
}

// ---- SFTP sidecar ----

const (
	// SFTPServiceName is the per-server NodePort Service exposing SFTP.
	SFTPServiceName = "server-sftp"
	// SFTPPort is the in-pod port the SFTP sidecar listens on.
	SFTPPort int32 = 2022
	// SFTPHostKeySecret holds the (stable) SSH host private key.
	SFTPHostKeySecret = "quetzal-sftp-host"
	// SFTPHostKeyField is the key within that Secret.
	SFTPHostKeyField = "host_key"
	// SFTPAuthKeysConfigMap holds the authorized_keys for the server.
	SFTPAuthKeysConfigMap = "quetzal-sftp-authorized"
	// SFTPAuthKeysField is the key within that ConfigMap.
	SFTPAuthKeysField = "authorized_keys"

	sftpBinPath    = "/quetzal-bin/sftp"
	sftpHostKeyVol = "sftp-hostkey"
	sftpAuthKeyVol = "sftp-authkeys"
	sftpSecretDir  = "/quetzal-sftp"
)

// sftpCopyInitContainer installs the sftp binary from the Quetzal image onto the
// shared bin volume (so the sidecar can run it from the game image).
func sftpCopyInitContainer(systemImage string, t *models.Template) corev1.Container {
	return corev1.Container{
		Name:            "sftp-copy",
		Image:           systemImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/usr/local/bin/quetzal-sftp"},
		Env:             []corev1.EnvVar{{Name: "QUETZAL_INSTALL_TO", Value: sftpBinPath}},
		VolumeMounts:    []corev1.VolumeMount{{Name: renderBinVolume, MountPath: renderBinMount}},
		SecurityContext: buildContainerSecurityContext(t),
	}
}

// sftpSidecar runs the SFTP server from the game image (same user as the server)
// against the data volume, with the host key and authorized_keys mounted.
func sftpSidecar(s *models.Server, t *models.Template, dataPath string) corev1.Container {
	return corev1.Container{
		Name:            "sftp",
		Image:           s.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{sftpBinPath},
		Env: []corev1.EnvVar{
			{Name: "QUETZAL_SFTP_ADDR", Value: fmt.Sprintf(":%d", SFTPPort)},
			{Name: "QUETZAL_DATA_PATH", Value: dataPath},
			{Name: "QUETZAL_SFTP_HOST_KEY", Value: sftpSecretDir + "/hostkey/" + SFTPHostKeyField},
			{Name: "QUETZAL_SFTP_AUTHORIZED_KEYS", Value: sftpSecretDir + "/auth/" + SFTPAuthKeysField},
		},
		Ports: []corev1.ContainerPort{{Name: "sftp", ContainerPort: SFTPPort, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolume, MountPath: dataPath},
			{Name: renderBinVolume, MountPath: renderBinMount, ReadOnly: true},
			{Name: sftpHostKeyVol, MountPath: sftpSecretDir + "/hostkey", ReadOnly: true},
			{Name: sftpAuthKeyVol, MountPath: sftpSecretDir + "/auth", ReadOnly: true},
		},
		// Gate pod readiness on the listener actually being bound.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(SFTPPort)},
			},
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		SecurityContext: buildContainerSecurityContext(t),
	}
}

func sftpVolumes() []corev1.Volume {
	return []corev1.Volume{
		{
			Name:         sftpHostKeyVol,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: SFTPHostKeySecret}},
		},
		{
			Name: sftpAuthKeyVol,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SFTPAuthKeysConfigMap},
			}},
		},
	}
}

// BuildSFTPService is the NodePort Service exposing a server's SFTP sidecar.
// The NodePort is assigned by Kubernetes (not from Quetzal's game-port pool).
func BuildSFTPService(s *models.Server) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: SFTPServiceName, Namespace: s.Namespace, Labels: labelsFor(s)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{serverLabel: s.Slug},
			Ports: []corev1.ServicePort{{
				Name: "sftp", Port: SFTPPort, TargetPort: intstr.FromInt32(SFTPPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// BuildSFTPAuthKeysConfigMap renders authorized_keys from the given public keys.
func BuildSFTPAuthKeysConfigMap(s *models.Server, keys []string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: SFTPAuthKeysConfigMap, Namespace: s.Namespace, Labels: labelsFor(s)},
		Data:       map[string]string{SFTPAuthKeysField: strings.Join(keys, "\n") + "\n"},
	}
}

// primaryPort returns the server's primary port (or first, or 0).
func primaryPort(s *models.Server, t *models.Template) int32 {
	ports := serverPorts(s, t)
	for _, p := range ports {
		if p.Primary {
			return p.Port
		}
	}
	if len(ports) > 0 {
		return ports[0].Port
	}
	return 0
}

// pbVarRe matches a {{...}} placeholder in an egg config.files value.
var pbVarRe = regexp.MustCompile(`{{\s*([^}]+?)\s*}}`)

// toShellTemplate converts an egg config.files value's placeholders into a form
// the renderer expands against the container env: env references become ${VAR}
// (so secrets resolve at runtime, not baked into the spec) and the default port
// is substituted literally. Unknown placeholders are left untouched.
func toShellTemplate(v string, primary int32) string {
	return pbVarRe.ReplaceAllStringFunc(v, func(m string) string {
		inner := strings.TrimSpace(pbVarRe.FindStringSubmatch(m)[1])
		switch {
		case inner == "server.build.default.port":
			return strconv.Itoa(int(primary))
		case inner == "server.build.default.ip":
			return "0.0.0.0"
		case strings.HasPrefix(inner, "server.build.env."):
			return "${" + strings.TrimPrefix(inner, "server.build.env.") + "}"
		case strings.HasPrefix(inner, "env."):
			return "${" + strings.TrimPrefix(inner, "env.") + "}"
		case identRe.MatchString(inner):
			return "${" + inner + "}"
		default:
			return m
		}
	})
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// buildInstallScript wraps a template's install script with the
// generation-marker logic: skip when the marker already records the current
// generation; treat a legacy/empty marker (generation 0) as installed so
// upgrading never re-runs install on existing servers; on a generation mismatch
// (reinstall) optionally wipe the volume, run the script, then record the new
// generation. QUETZAL_INSTALL_GEN / QUETZAL_INSTALL_WIPE are passed as env.
func buildInstallScript(mount, userScript string) string {
	return fmt.Sprintf(`marker="%[1]s/.quetzal-installed"
if [ -f "$marker" ]; then
  cur="$(cat "$marker" 2>/dev/null)"
  [ "$cur" = "$QUETZAL_INSTALL_GEN" ] && exit 0
  { [ "$QUETZAL_INSTALL_GEN" = "0" ] || [ -z "$QUETZAL_INSTALL_GEN" ]; } && exit 0
fi
if [ "$QUETZAL_INSTALL_WIPE" = "1" ]; then
  rm -rf "%[1]s/"* "%[1]s/".[!.]* 2>/dev/null || true
fi
%[2]s
printf '%%s' "$QUETZAL_INSTALL_GEN" > "$marker"
`, mount, userScript)
}

func installInitContainers(s *models.Server, t *models.Template) []corev1.Container {
	if t.Install == nil || t.Install.Script == "" {
		return nil
	}
	image := t.Install.Image
	if image == "" {
		image = "alpine:3.20"
	}
	entrypoint := t.Install.Entrypoint
	if entrypoint == "" {
		entrypoint = "sh"
	}
	wrapped := buildInstallScript(installMountPath, t.Install.Script)
	wipe := "0"
	if s.InstallWipe {
		wipe = "1"
	}
	return []corev1.Container{{
		Name:            "install",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{entrypoint, "-c", wrapped},
		Env: []corev1.EnvVar{
			{Name: "QUETZAL_INSTALL_GEN", Value: strconv.Itoa(s.InstallGeneration)},
			{Name: "QUETZAL_INSTALL_WIPE", Value: wipe},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: dataVolume, MountPath: installMountPath}},
	}}
}

var startupVarRe = regexp.MustCompile(`{{\s*([A-Za-z_][A-Za-z0-9_]*)\s*}}`)

// startupCommand builds the container command from a template's startup string,
// substituting {{VAR}} with the shell ${VAR} so env values expand at runtime.
// Returns nil when no startup is defined (use the image entrypoint).
func startupCommand(t *models.Template) []string {
	if t.Startup == "" {
		return nil
	}
	cmd := startupVarRe.ReplaceAllString(t.Startup, "${$1}")
	return []string{"/bin/sh", "-c", cmd}
}

func protocol(p string) corev1.Protocol {
	if p == "UDP" {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// NamespaceFor returns the conventional namespace name for a server slug.
func NamespaceFor(slug string) string {
	return fmt.Sprintf("quetzal-srv-%s", slug)
}
