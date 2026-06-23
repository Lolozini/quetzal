package reconciler

import (
	"context"
	"fmt"
	"log"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
)

const (
	// DefaultMariaDBImage backs a managed database host when none is specified.
	DefaultMariaDBImage = "mariadb:11.4"
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

// ManagedDBNamespace returns the namespace a managed host's workload lives in.
func ManagedDBNamespace(h *models.DatabaseHost) string {
	if h.Namespace != "" {
		return h.Namespace
	}
	return fmt.Sprintf("quetzal-db-%d", h.ID)
}

// ManagedDBServiceHost returns the in-cluster DNS name servers use to reach a
// managed host (the address handed to game servers).
func ManagedDBServiceHost(h *models.DatabaseHost) string {
	return fmt.Sprintf("%s.%s.svc", ManagedDBServiceName, ManagedDBNamespace(h))
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
	for i := range hosts {
		h := &hosts[i]
		if h.Kind != models.DBHostManaged {
			continue
		}
		ns := ManagedDBNamespace(h)
		valid[ns] = true
		rootPw, err := r.Store.DatabaseHostAdminPassword(h)
		if err != nil {
			log.Printf("db host %d: read root password: %v", h.ID, err)
			continue
		}
		for _, obj := range buildManagedDB(h, rootPw) {
			if err := r.apply(ctx, obj); err != nil {
				log.Printf("db host %d: apply %T: %v", h.ID, obj, err)
			}
		}
	}
	return r.gcManagedDBNamespaces(ctx, valid)
}

// gcManagedDBNamespaces deletes managed-database namespaces whose host row is
// gone (mirrors GCOrphanNamespaces for servers).
func (r *Reconciler) gcManagedDBNamespaces(ctx context.Context, valid map[string]bool) error {
	var list corev1.NamespaceList
	if err := r.Client.List(ctx, &list, client.MatchingLabels{dbComponentLabel: dbComponentValue}); err != nil {
		return err
	}
	for i := range list.Items {
		ns := &list.Items[i]
		if valid[ns.Name] || ns.DeletionTimestamp != nil {
			continue
		}
		if err := r.Client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// buildManagedDB returns the objects backing a managed MariaDB host.
func buildManagedDB(h *models.DatabaseHost, rootPassword string) []client.Object {
	ns := ManagedDBNamespace(h)
	labels := managedDBLabels(h)
	image := h.Image
	if image == "" {
		image = DefaultMariaDBImage
	}
	size := h.StorageSize
	if size == "" {
		size = "1Gi"
	}
	selector := map[string]string{dbHostLabel: strconv.FormatUint(uint64(h.ID), 10)}
	replicas := int32(1)
	noAutomount := false

	namespace := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels},
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
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	// A TCP probe on the MySQL port: mariadbd only starts listening once the data
	// directory is initialized, so "port open" is a sound readiness signal and
	// avoids healthcheck.sh's need for credentials. The generous threshold covers
	// a slow first-time initialization.
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(ManagedDBPort)},
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
	return []client.Object{namespace, secret, pvc, deploy, svc}
}
