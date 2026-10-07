package reconciler

import (
	"context"
	"fmt"
	"log"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
)

const (
	// DefaultMariaDBImage backs a managed database host when none is specified,
	// and holds the client tools that dump and load an external host's
	// databases. 12.3 is MariaDB's current LTS, maintained until June 2029. It
	// names the release line, not "lts": that tag moves to the next LTS when
	// one comes out, and a host would then change major version on its next
	// restart, with nobody having asked. A host keeps the image it was created
	// with.
	DefaultMariaDBImage = "mariadb:12.3"
	// ManagedDBServiceName is the in-cluster Service (and Deployment) name of a
	// managed database host within its namespace.
	ManagedDBServiceName = "quetzal-db"
	// ManagedDBPort is the MariaDB port.
	ManagedDBPort int32 = 3306
	// dbComponentLabel marks managed-database objects (for GC).
	dbComponentLabel = "app.kubernetes.io/component"
	dbComponentValue = "database"
	// dbHostLabel carries the DatabaseHost ID on managed-database objects.
	dbHostLabel    = "quetzal.dev/db-host"
	dbRootSecret   = "quetzal-db-root"
	dbRootField    = "root-password"
	dbDataVolume   = "data"
	dbDataPath     = "/var/lib/mysql"
	dbRootPassword = "MARIADB_ROOT_PASSWORD"
)

// ManagedDBServiceHost returns the in-cluster DNS name servers use to reach a
// managed host (the address handed to game servers).
func ManagedDBServiceHost(h *models.DatabaseHost) string {
	return fmt.Sprintf("%s.%s.svc", ManagedDBServiceName, h.ManagedNamespace())
}

func managedDBLabels(h *models.DatabaseHost) map[string]string {
	return map[string]string{
		managedByLabel:   managedByValue,
		dbComponentLabel: dbComponentValue,
		dbHostLabel:      strconv.FormatUint(uint64(h.ID), 10),
	}
}

// ReconcileDatabaseHosts brings managed (Quetzal-owned) database hosts to their
// desired state: a namespace, a root-password Secret, a PVC, a MariaDB
// Deployment and a ClusterIP Service per host. Namespaces of managed hosts that
// no longer exist in the DB are garbage-collected. Managed hosts run on the
// local cluster. External hosts are ignored (Quetzal only provisions on them).
func (r *Reconciler) ReconcileDatabaseHosts(ctx context.Context) error {
	hosts, err := r.Store.ListDatabaseHosts()
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	counts := map[string]int{}
	for i := range hosts {
		if hosts[i].Kind == models.DBHostManaged {
			counts[hosts[i].ManagedNamespace()]++
		}
	}
	for i := range hosts {
		h := &hosts[i]
		if h.Kind != models.DBHostManaged {
			continue
		}
		ns := h.ManagedNamespace()
		valid[ns] = true
		if counts[ns] != 1 {
			log.Printf("db host %d: namespace %q is reserved by multiple legacy hosts; refusing to reconcile", h.ID, ns)
			continue
		}
		rootPw, err := r.Store.DatabaseHostAdminPassword(h)
		if err != nil {
			log.Printf("db host %d: read root password: %v", h.ID, err)
			continue
		}
		objs, err := buildManagedDB(h, rootPw, r.InstanceID)
		if err != nil {
			log.Printf("db host %d: build: %v", h.ID, err)
			continue
		}
		// Do not apply the namespace with ForceOwnership: an existing owner's
		// label must be checked before any mutation, including the RoleBinding.
		if err := r.ensureOwnedNamespace(ctx, ns, managedDBLabels(h)); err != nil {
			log.Printf("db host %d: namespace: %v", h.ID, err)
			continue
		}
		r.ensureRoleBinding(ctx, ns)
		for _, obj := range objs[1:] {
			if err := r.apply(ctx, obj); err != nil {
				log.Printf("db host %d: apply %T: %v", h.ID, obj, err)
			}
		}
		r.ensureManagedDBNetworkPolicy(ctx, h)
	}
	return r.gcManagedDBNamespaces(ctx, valid)
}

// ensureManagedDBNetworkPolicy keeps a managed database's ingress policy in step
// with the servers that hold a database on it. It is rewritten on every resync,
// so a database added or dropped between two passes is reflected on the next
// one — the same convergence the servers' own egress rules rely on.
func (r *Reconciler) ensureManagedDBNetworkPolicy(ctx context.Context, h *models.DatabaseHost) {
	if r.ControlPlaneNamespace == "" {
		r.warnOnce("db-netpol-no-namespace",
			"managed databases are left reachable from anywhere in the cluster: the control plane's own namespace is unknown (POD_NAMESPACE), and a policy without it would block provisioning")
		return
	}
	namespaces, err := r.Store.ServerNamespacesUsingHost(h.ID)
	if err != nil {
		// Rewriting the policy from an incomplete list would cut live servers off
		// from their database. Leave the last good one in place and retry.
		log.Printf("db host %d: list server namespaces (its ingress policy is left as it is): %v", h.ID, err)
		return
	}
	if err := r.apply(ctx, BuildManagedDBNetworkPolicy(h, append(namespaces, r.ControlPlaneNamespace))); err != nil {
		log.Printf("db host %d: apply ingress policy: %v", h.ID, err)
	}
}

// BuildManagedDBNetworkPolicy restricts who may open a connection to a managed
// MariaDB: the servers that have a database on it, and the control plane, which
// provisions them. Nothing else in the cluster has any business on 3306.
//
// Without it the database is reachable by every pod that can route to its
// namespace — other tenants' servers, and any unrelated workload sharing the
// cluster. MySQL grants are still what keeps one tenant out of another's tables,
// and they hold; this removes the chance to try at all, and to reach a MariaDB
// that is unpatched or misconfigured.
//
// Only ingress is constrained. The pod's own egress is left alone: it needs DNS
// and nothing else, and a policy there would be one more thing to get wrong on a
// component that initiates no connections.
//
// The readiness probe is a connection from the kubelet, which NetworkPolicy does
// not govern — verified on Cilium, where the pod goes Ready under a policy that
// names neither the node nor its namespace, while a pod in an unlisted namespace
// is refused. Naming it here because the symptom on a CNI that did enforce it
// would be a database that never becomes ready, which reads like anything but a
// policy problem.
func BuildManagedDBNetworkPolicy(h *models.DatabaseHost, allowed []string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(ManagedDBPort)
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(allowed))
	for _, ns := range allowed {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			// kubernetes.io/metadata.name is set by the API server on every
			// namespace, so this needs no label of Quetzal's own to be applied
			// first — and cannot be spoofed by labelling a namespace.
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			},
		})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quetzal-db-ingress",
			Namespace: h.ManagedNamespace(),
			Labels:    managedDBLabels(h),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: managedDBLabels(h)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  peers,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
}

// gcManagedDBNamespaces deletes managed-database namespaces whose host row is
// gone (mirrors GCOrphanNamespaces for servers, including its ownership rule:
// another control plane's database namespace looks orphaned here while being
// live there, and dropping it would destroy that data).
func (r *Reconciler) gcManagedDBNamespaces(ctx context.Context, valid map[string]bool) error {
	if r.InstanceID == "" {
		return nil
	}
	var list corev1.NamespaceList
	if err := r.Client.List(ctx, &list, client.MatchingLabels{dbComponentLabel: dbComponentValue}); err != nil {
		return err
	}
	for i := range list.Items {
		ns := &list.Items[i]
		if valid[ns.Name] || ns.DeletionTimestamp != nil {
			continue
		}
		if owner := ns.Labels[InstanceLabel]; owner != "" && owner != r.InstanceID {
			continue // belongs to another control plane
		}
		// The label is Quetzal's, but it can end up on a namespace Quetzal did not
		// create — the name came from an API request once. Deleting on the label
		// alone would take kube-system with it.
		if !models.IsManagedDBNamespace(ns.Name) {
			log.Printf("refusing to collect namespace %q: it carries the managed-database label but is not a name Quetzal would have chosen", ns.Name)
			continue
		}
		if err := r.Client.Delete(ctx, ns, client.Preconditions{UID: &ns.UID, ResourceVersion: &ns.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// buildManagedDB returns the objects backing a managed MariaDB host. instanceID
// stamps the namespace with the owning control plane so orphan collection never
// reclaims (and destroys) another instance's databases; "" leaves it unstamped,
// which collection then adopts.
func buildManagedDB(h *models.DatabaseHost, rootPassword, instanceID string) ([]client.Object, error) {
	ns := h.ManagedNamespace()
	labels := managedDBLabels(h)
	nsLabels := managedDBLabels(h)
	if instanceID != "" {
		nsLabels[InstanceLabel] = instanceID
	}
	image := h.Image
	if image == "" {
		image = DefaultMariaDBImage
	}
	size := h.StorageSize
	if size == "" {
		size = "1Gi"
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil || quantity.Sign() <= 0 {
		return nil, fmt.Errorf("invalid storage size %q: expected a positive Kubernetes quantity", size)
	}
	selector := map[string]string{dbHostLabel: strconv.FormatUint(uint64(h.ID), 10)}
	replicas := int32(1)
	noAutomount := false

	namespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: nsLabels},
	}
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: dbRootSecret, Namespace: ns, Labels: labels},
		Data:       map[string][]byte{dbRootField: []byte(rootPassword)},
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: dbDataVolume, Namespace: ns, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
	// The image's own health check, which connects over the local socket as the
	// mysql user (unix_socket auth, so no credentials) and asks whether InnoDB
	// is up. It was a TCP probe on the MySQL port, which opens a connection and
	// closes it without authenticating: MariaDB logs a warning for each one, and
	// two probes every ten seconds filled the log with nothing else -- about
	// 17 000 lines a day, which is where a real problem would have been. This
	// also answers a sounder question, since the port is open while InnoDB is
	// still recovering. The generous threshold covers a slow first-time
	// initialization.
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{
				"healthcheck.sh", "--su-mysql", "--connect", "--innodb_initialized",
			}},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		TimeoutSeconds:      5,
		FailureThreshold:    30,
	}
	deploy := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ManagedDBServiceName, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: mergeLabels(labels, selector)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &noAutomount,
					Containers: []corev1.Container{{
						Name:  "mariadb",
						Image: image,
						Ports: []corev1.ContainerPort{{Name: "mysql", ContainerPort: ManagedDBPort}},
						Env: []corev1.EnvVar{{
							Name: dbRootPassword,
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: dbRootSecret},
								Key:                  dbRootField,
							}},
						}},
						VolumeMounts:   []corev1.VolumeMount{{Name: dbDataVolume, MountPath: dbDataPath}},
						ReadinessProbe: probe,
						LivenessProbe:  probe,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
						},
						// No hardened securityContext here on purpose: the official
						// MariaDB entrypoint runs as root to chown the data dir and
						// gosu down to the mysql user, so dropping capabilities would
						// break initialization. This is a trusted, Quetzal-owned image
						// (not untrusted game code); the pod still mounts no SA token.
					}},
					Volumes: []corev1.Volume{{
						Name: dbDataVolume,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dbDataVolume},
						},
					}},
				},
			},
		},
	}
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ManagedDBServiceName, Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name: "mysql", Port: ManagedDBPort, TargetPort: intstr.FromInt32(ManagedDBPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	return []client.Object{namespace, secret, pvc, deploy, svc}, nil
}
