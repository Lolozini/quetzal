package reconciler

import (
	"fmt"
	"regexp"
	"sort"

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
	// WorkloadName is the Deployment/Service name within a server's namespace.
	WorkloadName  = "server"
	serverLabel   = ServerLabel
	workloadName  = WorkloadName
	dataVolume    = "data"       // PVC / volume name
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
// exposure (ClusterIP by default, NodePort or LoadBalancer when published).
func BuildService(s *models.Server, t *models.Template) *corev1.Service {
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
			Selector: map[string]string{serverLabel: s.Slug},
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
