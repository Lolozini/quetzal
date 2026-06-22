package reconciler

import (
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

	"github.com/lolozini/quetzal/internal/models"
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

// BuildPVC returns the data PersistentVolumeClaim (nil for hostPath storage).
func BuildPVC(s *models.Server) *corev1.PersistentVolumeClaim {
	if s.Storage.Type != models.StoragePVC {
		return nil
	}
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
func BuildDeployment(s *models.Server, t *models.Template, secretKeys []string) *appsv1.Deployment {
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

	pod := corev1.PodSpec{
		Containers:                    []corev1.Container{container},
		InitContainers:                installInitContainers(t),
		SecurityContext:               buildPodSecurityContext(t),
		Volumes:                       []corev1.Volume{buildDataVolume(s)},
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
	if s.Storage.Type == models.StorageHostPath {
		hpType := corev1.HostPathDirectoryOrCreate
		return corev1.Volume{
			Name: dataVolume,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: s.Storage.HostPath,
					Type: &hpType,
				},
			},
		}
	}
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
func installInitContainers(t *models.Template) []corev1.Container {
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
	script := fmt.Sprintf("if [ -f %[1]s/.quetzal-installed ]; then exit 0; fi\n%[2]s\ntouch %[1]s/.quetzal-installed\n",
		installMountPath, t.Install.Script)
	return []corev1.Container{{
		Name:            "install",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{entrypoint, "-c", script},
		VolumeMounts:    []corev1.VolumeMount{{Name: dataVolume, MountPath: installMountPath}},
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
