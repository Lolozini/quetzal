// Package reconciler projects servers (the DB source of truth) into native
// Kubernetes objects, and writes observed status back to the DB.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lolozini/quetzal/internal/authkeys"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/startup"
	"github.com/lolozini/quetzal/internal/store"
)

// Reconciler turns desired DB state into Kubernetes objects.
type Reconciler struct {
	Client client.Client
	Store  *store.Store
	// lookupState resolves the names of external database hosts (dbegress.go).
	lookupState

	// NamespacedRole is the ClusterRole the control plane binds to itself in each
	// namespace it creates, instead of holding that access cluster-wide. Empty
	// disables the binding, which is what an install predating the split, or a
	// cluster registered with an administrator's kubeconfig, looks like.
	NamespacedRole string

	// warned de-duplicates warnings that would otherwise repeat every resync.
	warnMu sync.Mutex
	warned map[string]bool

	// identity* cache who this control plane authenticates as on this cluster.
	identityMu      sync.Mutex
	identityDone    bool
	identityOK      bool
	identitySubject rbacv1.Subject

	// accessWait bounds the wait for a new RoleBinding to take effect (0: the
	// default); tests shorten it.
	accessWait time.Duration

	// OnStop, if set, is called just before a running server is scaled to zero
	// so a graceful stop command can be delivered to the container (via the
	// console attach path). It is best-effort. Injected by the controller to
	// avoid an import cycle with the console package.
	OnStop func(ctx context.Context, namespace, slug, stopCommand string) error

	// LogTail, if set, returns the last lines of a container's log, its previous
	// run's when previous is true. A crash's message quotes the error it ends
	// with. Injected by the controller, which holds the clientset logs need.
	LogTail func(ctx context.Context, namespace, pod, container string, previous bool, lines int64) (string, error)

	// StartupSeen, if set, reports whether a game container has printed one of
	// its template's done lines, following its output in the background until it
	// does or deadline passes (see the startup package). Injected by the
	// controller, which holds the clientset a log stream needs. Unset, a ready
	// container counts as started.
	StartupSeen func(namespace, pod, containerID string, deadline time.Time, ms []startup.Matcher) bool

	// Wake-on-connect: when ActivatorImage is set, a server with wake-on-connect
	// (drop) or proxy mode gets an activator. WakeURL/ActiveURL are the control
	// plane callbacks; WakeKey signs the per-server callback token.
	ActivatorImage string
	WakeURL        string
	ActiveURL      string
	WakeKey        []byte

	// ExtraEgressCIDRs are private ranges every server may reach, on top of the
	// public internet. The default policy denies private address space so the
	// cluster network is out of reach; an operator whose servers need something
	// there — an external database named by DNS, a license server on the LAN —
	// names its range here. Injected by the controller from QUETZAL_EGRESS_ALLOW.
	ExtraEgressCIDRs []string

	// DNSServers are the resolver addresses the controller itself was given
	// (Nameservers), which a server may query on port 53 besides the cluster's
	// DNS pods and node caches: see clusterDNSPeers.
	DNSServers []string

	// ControlPlaneNamespace is where the apiserver and controller run. A managed
	// database's ingress policy has to let it in: it is the one that creates and
	// drops databases over 3306. Empty means unknown, and the policy is then not
	// written at all rather than written in a way that would lock provisioning
	// out — a database nobody can provision is a worse outcome than one reachable
	// from inside the cluster, which is what it was before.
	ControlPlaneNamespace string

	// NodePortMin/NodePortMax bound the node-port pool the SFTP Service draws
	// from (0 = the store's defaults). Same pool as the game ports, so SFTP and
	// game allocations never collide. Injected by the controller.
	NodePortMin int32
	NodePortMax int32

	// ClusterID is the cluster this reconciler drives. It selects that cluster's
	// endpoint hostname, since each cluster has its own nodes and a single
	// panel-wide name would advertise one cluster's address for another's
	// servers. 0 = the control plane's own cluster.
	ClusterID uint

	// InstanceID identifies this control plane (see InstanceLabel). Namespaces
	// are stamped with it, and orphan collection only reclaims namespaces that
	// are unowned or owned by this instance — two control planes sharing a
	// cluster would otherwise delete each other's servers. Collection is a no-op
	// while this is empty.
	InstanceID string
}

// New returns a Reconciler. It resolves the control plane's instance id from the
// store so ownership stamping and orphan collection are correct by construction;
// if that read fails, InstanceID stays empty and collection is skipped rather
// than risking another instance's namespaces.
func New(c client.Client, s *store.Store) *Reconciler {
	r := &Reconciler{Client: c, Store: s}
	if id, err := s.InstanceID(); err == nil {
		r.InstanceID = id
	}
	return r
}

// ReconcileServer ensures the cluster matches the DB for one server, then
// updates its status in the DB.
func (r *Reconciler) ReconcileServer(ctx context.Context, id uint) error {
	srv, err := r.Store.GetServer(id)
	if err != nil {
		return err
	}
	tmpl, err := r.Store.GetTemplate(srv.TemplateID)
	if err != nil {
		return fmt.Errorf("server %s: template: %w", srv.Slug, err)
	}
	// A server's namespace is derived from its slug at creation and never changes
	// afterwards, so the two can only disagree if the row was tampered with —
	// which would aim everything below, up to and including namespace deletion,
	// at whatever the row said. Refuse rather than derive: if this ever fires it
	// is either an attack or a schema change, and both want a human.
	if want := NamespaceFor(srv.Slug); srv.Namespace != want {
		return fmt.Errorf("server %s: namespace %q does not match its slug (expected %q); refusing to act on it",
			srv.Slug, srv.Namespace, want)
	}

	tmpl, pinned := r.renderedTemplate(ctx, srv, tmpl)

	// A step that fails stops the ones after it, but no longer the status: it
	// used to, and the panel went on showing whatever it showed when a pass
	// last went through -- "Stopped", next to a game that was running, while
	// the controller's log was the only place to say its Service was refused.
	if err := r.ensureObjects(ctx, srv, tmpl); err != nil {
		if stErr := r.writeStatus(ctx, srv, tmpl, joinNotice(pinned, failureNotice(err))); stErr != nil {
			log.Printf("server %s: status: %v", srv.Slug, stErr)
		}
		return err
	}
	return r.writeStatus(ctx, srv, tmpl, pinned)
}

// renderedTemplate is the template a server's pods are made from, and a notice
// when that is not the current one. A server whose game is up keeps the
// version it started with: an edited or re-imported template used to replace
// the pod of every running server that used it, at once and players and all.
// It takes the current version the next time its pod goes anyway -- a stop, a
// restart, hibernation, a crash, a change to its own settings -- or when it is
// reinstalled. A version kept nowhere (the server predates revisions, and runs
// the current one already) is taken at once.
func (r *Reconciler) renderedTemplate(ctx context.Context, srv *models.Server, cur *models.Template) (*models.Template, string) {
	if srv.TemplateVersion == cur.Version {
		return cur, ""
	}
	if srv.Replicas() > 0 {
		if old, err := r.Store.GetTemplateRevision(cur.ID, srv.TemplateVersion); err == nil && r.keepsRunning(ctx, srv, old) {
			return old, fmt.Sprintf("its template was updated (version %d): the server takes it at its next restart", cur.Version)
		}
	}
	if err := r.Store.SetServerTemplateVersion(srv.ID, cur.ID, cur.Version); err != nil {
		log.Printf("server %s: move to template version %d: %v", srv.Slug, cur.Version, err)
	} else {
		srv.TemplateVersion = cur.Version
	}
	return cur, ""
}

// keepsRunning reports whether the server's game is up on a pod that old, the
// template version it started with, still renders as it is: nothing else about
// the server changed, and only the template's update would replace the pod.
// When something did, the pod goes anyway, and had better take the current
// template in the same stroke than be replaced a second time for it.
func (r *Reconciler) keepsRunning(ctx context.Context, srv *models.Server, old *models.Template) bool {
	live := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: workloadName}, live); err != nil {
		return false
	}
	if !scaledUp(live) || live.Status.ReadyReplicas == 0 {
		return false
	}
	secretEnv, err := r.Store.OpenSecrets(srv.SecretEnvEnc)
	if err != nil {
		return false
	}
	keys := make([]string, 0, len(secretEnv))
	for k := range secretEnv {
		keys = append(keys, k)
	}
	want := BuildDeployment(srv, old, r.ActivatorImage, keys)
	keepHelperImage(live, want, r.ActivatorImage)
	return !rolls(live, want)
}

// ensureObjects brings a server's Kubernetes objects in line with its row, one
// after the other, and stops at the first that fails.
func (r *Reconciler) ensureObjects(ctx context.Context, srv *models.Server, tmpl *models.Template) error {
	if err := r.ensureNamespace(ctx, srv); err != nil {
		return failed("namespace", err)
	}
	// Before anything else in there: every step below needs the access this
	// grants, and on an upgrade the namespace already exists without it.
	r.ensureRoleBinding(ctx, srv.Namespace)
	// The network policy is what keeps the game's code -- a tenant's mods and
	// plugins -- off the cluster network, so nothing that runs that code may
	// exist without it. It used to come last, after the Service: a Service the
	// cluster refused (a node port something else already held) stopped the
	// pass before it, on every pass, and the game ran with the run of the
	// cluster, the panel and the API server included. It depends on nothing
	// created below, so it goes first, and a policy that cannot be written
	// stops everything else.
	if err := r.ensureNetworkPolicy(ctx, srv, tmpl); err != nil {
		return failed("network policy", err)
	}
	if err := r.ensureResourceQuota(ctx, srv); err != nil {
		return failed("resource quota", err)
	}
	// SFTP supporting objects (host key, authorized_keys, Service) must exist
	// before the Deployment references them.
	if err := r.ensureSFTP(ctx, srv); err != nil {
		return failed("SFTP access", err)
	}
	if pvc := BuildPVC(srv); pvc != nil {
		if err := r.ensurePVC(ctx, pvc); err != nil {
			return failed("data volume", err)
		}
	}

	// Materialize sensitive env into a per-server Secret (referenced by the
	// Deployment via secretKeyRef). Values are decrypted from the DB here.
	secretEnv, err := r.Store.OpenSecrets(srv.SecretEnvEnc)
	if err != nil {
		return failed("secret variables", err)
	}
	if sec := BuildSecret(srv, secretEnv); sec != nil {
		if err := r.ensureSecret(ctx, sec); err != nil {
			return failed("secret variables", err)
		}
	}
	secretKeys := make([]string, 0, len(secretEnv))
	for k := range secretEnv {
		secretKeys = append(secretKeys, k)
	}
	// A restart is a stop, its stop command included, and a start once the
	// game's pod is gone. It used to delete the pod: the game got SIGTERM and
	// nothing else, and the Deployment's next pod could start on the volume
	// while the old one was still saving to it.
	if srv.RestartRequestedAt != nil && r.restartDone(ctx, srv, tmpl) {
		if err := r.Store.FinishRestart(srv.ID); err != nil {
			log.Printf("server %s: finish restart: %v", srv.Slug, err)
		} else {
			srv.RestartRequestedAt = nil
		}
	}
	// Graceful stop: when a server that is up is about to be scaled to zero --
	// stopped, suspended, or put to sleep by hibernation -- and the template
	// defines a stop command, deliver it first (SIGTERM + the termination grace
	// period follow). Hibernation used to be left out, because a hibernated
	// server is still desired Running: its game only got the SIGTERM, which a
	// startup wrapped in a shell never passes on, so the world went down with
	// whatever it had not saved.
	if needsGracefulStop(srv, tmpl) && r.OnStop != nil {
		if running, _ := r.deploymentRunning(ctx, srv.Namespace); running {
			if err := r.OnStop(ctx, srv.Namespace, srv.Slug, tmpl.StopCommand); err != nil {
				log.Printf("graceful stop for %s (continuing to scale down): %v", srv.Slug, err)
			}
		}
	}

	// The data-manager pod (files + SFTP) mounts the data volume permanently; the
	// game pod is co-located with it via podAffinity. Ensure it before the game
	// Deployment so the game pod has a node to anchor to.
	if err := r.ensureDataDeployment(ctx, srv, tmpl); err != nil {
		return failed("data manager", err)
	}

	// A reinstall's wipe is one-shot, but the flag used to stay set for good:
	// the next time the install step ran for that generation -- a restore that
	// rolled the install marker back, a marker deleted from the file manager --
	// it wiped the volume again, taking the restored data with it. Retire it
	// once the reinstall has been seen to come up, and only while nothing runs,
	// since the flag is part of the pod spec and changing it rolls the pod.
	if wipeConsumed(srv) {
		if err := r.Store.ClearInstallWipe(srv.ID); err != nil {
			log.Printf("server %s: clear install wipe: %v", srv.Slug, err)
		} else {
			srv.InstallWipe = false
		}
	}
	// The game pod mounts it, so it goes first.
	if cm := BuildPasswdConfigMap(srv, tmpl); cm != nil {
		if err := r.apply(ctx, cm); err != nil {
			return failed("passwd file", err)
		}
	}
	if err := r.ensureDeployment(ctx, srv, tmpl, secretKeys); err != nil {
		return failed("game Deployment", err)
	}
	// Wake-on-connect: an activator may front the server. In proxy mode it is
	// always in path (and needs an internal backend Service); in drop mode it
	// only appears while hibernated. The public Service selector points at the
	// activator when one is fronting, else at the real workload.
	proxy := r.proxyActive(srv, tmpl)
	drop := r.dropActive(srv, tmpl)
	if err := r.ensureInternalService(ctx, srv, tmpl, proxy); err != nil {
		return failed("internal Service", err)
	}
	if err := r.ensureActivator(ctx, srv, tmpl, proxy, drop); err != nil {
		return failed("activator", err)
	}
	// A Service requires at least one port; skip it for portless servers.
	if len(serverPorts(srv, tmpl)) > 0 {
		if err := r.ensureService(ctx, srv, tmpl, proxy || drop); err != nil {
			return failed("Service", err)
		}
	}
	return nil
}

// restartLimit bounds the stopping half of a restart, past the template's
// stop grace: a pod that has not gone by then is not going on its own, and the
// server is started again regardless.
const restartLimit = 5 * time.Minute

// restartDone reports whether a restart has stopped the game: none of its pods
// is left, or it has waited longer than any stop should take.
func (r *Reconciler) restartDone(ctx context.Context, srv *models.Server, t *models.Template) bool {
	limit := restartLimit + time.Duration(t.StopGraceSeconds)*time.Second
	if time.Since(*srv.RestartRequestedAt) > limit {
		log.Printf("server %s: its game was still not down %s into a restart; starting it again", srv.Slug, limit)
		return true
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(srv.Namespace), client.MatchingLabels{serverLabel: srv.Slug}); err != nil {
		return false
	}
	return len(pods.Items) == 0
}

// stepError is a step of a server's reconcile that did not go through.
type stepError struct {
	step string // what the step puts in place, as the status names it
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

func failed(step string, err error) error { return &stepError{step: step, err: err} }

// failureNotice is what a server's status says about a step that did not go
// through. What the API server answered is about the object and is shown; any
// other error is not -- a connection error names the cluster's address, which
// anyone allowed to see the server would read -- and is left to the log.
func failureNotice(err error) string {
	step := "objects"
	var se *stepError
	if errors.As(err, &se) {
		step = se.step
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if msg := status.Status().Message; msg != "" {
			return fmt.Sprintf("Kubernetes refused this server's %s: %s", step, msg)
		}
	}
	return fmt.Sprintf("this server's %s could not be put in place; the controller retries and logs why", step)
}

// needsGracefulStop reports whether a server's game should be sent its stop
// command before the workload scales down: it is going to zero replicas and the
// template's stop is console input rather than a signal.
func needsGracefulStop(s *models.Server, t *models.Template) bool {
	return s.Replicas() == 0 && isConsoleStop(t.StopCommand)
}

// wipeConsumed reports whether a reinstall's wipe has done its job and can be
// retired: the reinstall's generation has been seen running, and nothing runs
// now (the flag is in the pod spec, so clearing it on a live server would roll
// its pod).
func wipeConsumed(s *models.Server) bool {
	return s.InstallWipe && s.Replicas() == 0 && s.Status.InstalledGeneration >= s.InstallGeneration
}

// DeleteServer tears down a server by deleting its namespace (cascades).
func (r *Reconciler) DeleteServer(ctx context.Context, srv *models.Server) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: srv.Namespace}}
	if err := r.Client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// GCOrphanNamespaces deletes Quetzal-managed namespaces whose server slug is no
// longer present in the DB (i.e. the server row was removed). This provides
// teardown for deleted servers in the Phase 0 resync model.
//
// "Orphan" is relative to *this* control plane's database, so a namespace owned
// by another instance sharing the cluster is never touched — it looks orphaned
// here while being perfectly alive there. Ownership is read from InstanceLabel;
// an unlabelled namespace predates the label and is adopted, which keeps
// cleanup working for the ordinary single-instance install. Collection is
// skipped entirely when this reconciler has no instance id, since nothing could
// then be distinguished from another instance's namespaces.
func (r *Reconciler) GCOrphanNamespaces(ctx context.Context, validSlugs map[string]bool) error {
	if r.InstanceID == "" {
		return nil
	}
	var list corev1.NamespaceList
	if err := r.Client.List(ctx, &list, client.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		return err
	}
	for i := range list.Items {
		ns := &list.Items[i]
		slug := ns.Labels[serverLabel]
		if slug == "" || validSlugs[slug] {
			continue
		}
		if owner := ns.Labels[InstanceLabel]; owner != "" && owner != r.InstanceID {
			continue // belongs to another control plane
		}
		// The labels are Quetzal's own, but a label is not proof it created the
		// namespace — it writes them wherever a server row points. Deleting on
		// labels alone would follow a tampered row into someone else's namespace.
		if ns.Name != NamespaceFor(slug) {
			log.Printf("refusing to collect namespace %q: labelled for server %q, which belongs in %q", ns.Name, slug, NamespaceFor(slug))
			continue
		}
		if ns.DeletionTimestamp != nil {
			continue // already terminating
		}
		if err := r.Client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureNamespace(ctx context.Context, s *models.Server) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		ns.Labels = mergeLabels(ns.Labels, labelsFor(s))
		// Stamp ownership so orphan collection can tell this control plane's
		// namespaces from another's (see InstanceLabel). Also adopts namespaces
		// created before the label existed.
		if r.InstanceID != "" {
			ns.Labels[InstanceLabel] = r.InstanceID
		}
		return nil
	})
	if err == nil || !apierrors.IsForbidden(err) {
		return err
	}
	// Having the namespace is a hard requirement; re-labelling one that already
	// exists is not. This is the first step of a server's reconcile, so letting a
	// label write fail the whole call freezes everything after it — the
	// Deployment, the Service, the SFTP keys — while the panel still reports the
	// server as running. A role without update on namespaces is enough to trigger
	// it: adding any new label (as the ownership stamp did) turns a create-only
	// reconcile into an update on every pre-existing namespace.
	if getErr := r.Client.Get(ctx, client.ObjectKey{Name: s.Namespace}, &corev1.Namespace{}); getErr != nil {
		return err
	}
	r.warnOnce("namespace-labels", "cannot update namespace labels (%v); servers keep reconciling, but orphan-collection ownership will not be stamped on existing namespaces", err)
	return nil
}

// warnOnce logs a message the first time a given key is seen, so a condition
// that repeats on every 15s resync is reported without flooding the log.
func (r *Reconciler) warnOnce(key, format string, args ...any) {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	log.Printf(format, args...)
}

func (r *Reconciler) ensureResourceQuota(ctx context.Context, s *models.Server) error {
	return r.apply(ctx, BuildResourceQuota(s))
}

// ensureSFTP reconciles the SFTP sidecar's supporting objects: a stable host key
// (generated once), the authorized_keys ConfigMap (kept in sync with the users
// who hold file access), and the NodePort Service. When SFTP is disabled the
// Service and ConfigMap are removed (the host key is kept so re-enabling doesn't
// change it). Requires a system image (the SFTP binary lives there).
func (r *Reconciler) ensureSFTP(ctx context.Context, s *models.Server) error {
	if !s.SFTP.Enabled || r.ActivatorImage == "" {
		if !s.SFTP.Enabled {
			r.deleteSFTP(ctx, s)
		}
		return nil
	}
	if err := r.ensureSFTPHostKey(ctx, s); err != nil {
		return fmt.Errorf("sftp host key: %w", err)
	}
	keys, err := r.Store.ListAuthorizedSSHKeys(s.ID)
	if err != nil {
		return fmt.Errorf("sftp authorized keys: %w", err)
	}
	// Each key with the account it belongs to: SFTP lets a key in under its
	// account's name only, and its log says who did what.
	names := map[uint]string{}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		name, ok := names[k.UserID]
		if !ok {
			u, err := r.Store.GetUser(k.UserID)
			if err != nil {
				return fmt.Errorf("sftp authorized keys: %w", err)
			}
			name, names[k.UserID] = u.Username, u.Username
		}
		if line := authkeys.Line(k.PublicKey, []string{name}); line != "" {
			lines = append(lines, line)
		}
	}
	if err := r.apply(ctx, BuildSFTPAuthKeysConfigMap(s, lines)); err != nil {
		return fmt.Errorf("sftp configmap: %w", err)
	}
	// Draw the SFTP NodePort from the same pool as the game ports (stable per
	// server, no collision with Kubernetes' own auto-assignment).
	nodePort, err := r.Store.AllocateNodePort(s.ID, SFTPPortName, r.NodePortMin, r.NodePortMax)
	if err != nil {
		return fmt.Errorf("sftp node port: %w", err)
	}
	err = r.applyNodePortService(ctx,
		func() *corev1.Service { return BuildSFTPService(s, nodePort) },
		func(taken int32) error {
			np, err := r.moveSFTPPort(s, taken)
			if err == nil {
				nodePort = np
			}
			return err
		})
	if err != nil {
		return fmt.Errorf("sftp service: %w", err)
	}
	return nil
}

// ensureSFTPHostKey creates a stable SSH host key Secret if absent.
func (r *Reconciler) ensureSFTPHostKey(ctx context.Context, s *models.Server) error {
	key := client.ObjectKey{Namespace: s.Namespace, Name: SFTPHostKeySecret}
	if err := r.Client.Get(ctx, key, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	hostKey, err := crypto.GenerateSSHHostKey()
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SFTPHostKeySecret, Namespace: s.Namespace, Labels: labelsFor(s)},
		Data:       map[string][]byte{SFTPHostKeyField: hostKey},
	}
	if err := r.Client.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *Reconciler) deleteSFTP(ctx context.Context, s *models.Server) {
	_ = r.Client.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: SFTPServiceName, Namespace: s.Namespace}})
	_ = r.Client.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SFTPAuthKeysConfigMap, Namespace: s.Namespace}})
	// Return the SFTP node port to the pool so it can be reused.
	_ = r.Store.ReleaseNodePort(s.ID, SFTPPortName)
}

// ensureDataDeployment reconciles the always-on data-manager Deployment (files +
// SFTP). It normally runs one replica, but scales to zero while a restore is
// active for the server: a restore overwrites the data volume in place and needs
// exclusive write access, which it can't get while the data-manager holds the
// ReadWriteOnce mount. Once the restore finishes, the next reconcile brings it
// back.
func (r *Reconciler) ensureDataDeployment(ctx context.Context, s *models.Server, t *models.Template) error {
	replicas := int32(1)
	if active, err := r.Store.HasActiveRestore(s.ID); err != nil {
		return fmt.Errorf("check active restore: %w", err)
	} else if active {
		replicas = 0
	}
	return r.applyKeepingHelpers(ctx, BuildDataDeployment(s, t, r.ActivatorImage, replicas), r.ActivatorImage)
}

func (r *Reconciler) ensurePVC(ctx context.Context, want *corev1.PersistentVolumeClaim) error {
	// PVC spec is largely immutable: create if absent, otherwise leave as-is.
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, want)
	}
	return err
}

// fieldOwner identifies Quetzal in server-side-apply managed fields.
const fieldOwner = "quetzal-controller"

// apply performs a server-side apply. Unlike overwriting the whole spec on each
// reconcile, SSA is idempotent and leaves server-defaulted fields untouched, so
// unchanged objects produce no write churn.
func (r *Reconciler) apply(ctx context.Context, obj client.Object) error {
	return r.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

// ensureDeployment applies the game's Deployment. A change to its pod -- new
// resources, image, variables or ports, a reinstall -- replaces a running pod,
// and the game got SIGTERM alone, which a startup wrapped in a shell never
// passes on: it now gets its stop command first, as a stop gives it.
func (r *Reconciler) ensureDeployment(ctx context.Context, s *models.Server, t *models.Template, secretKeys []string) error {
	return r.applyRolling(ctx, BuildDeployment(s, t, r.ActivatorImage, secretKeys), r.ActivatorImage, func() {
		if r.OnStop == nil || !isConsoleStop(t.StopCommand) {
			return
		}
		if err := r.OnStop(ctx, s.Namespace, s.Slug, t.StopCommand); err != nil {
			log.Printf("stop command before replacing %s's pod (replacing it anyway): %v", s.Slug, err)
		}
	})
}

func (r *Reconciler) ensureService(ctx context.Context, s *models.Server, t *models.Template, activator bool) error {
	return r.applyNodePortService(ctx,
		func() *corev1.Service { return BuildService(s, t, activator) },
		func(taken int32) error { return r.moveGamePort(s, taken) })
}

// proxyActive reports whether the always-in-path proxy should front this server
// (hibernation + proxy mode + at least one port + an image to run).
func (r *Reconciler) proxyActive(s *models.Server, t *models.Template) bool {
	// Require a callback URL too: a proxy with no way to wake/heartbeat would let
	// the server hibernate with players and never wake. A server that is stopped
	// or suspended has nothing to front: its connections should be refused, not
	// accepted by a proxy with no backend.
	if r.ActivatorImage == "" || r.WakeURL == "" || !s.Hibernation.Enabled || !s.Hibernation.Proxy ||
		s.DesiredState != models.StateRunning {
		return false
	}
	return len(serverPorts(s, t)) > 0
}

// dropActive reports whether the lightweight wake-and-drop activator should
// front this server (hibernated + wake-on-connect, not proxy, a TCP port).
func (r *Reconciler) dropActive(s *models.Server, t *models.Template) bool {
	// Hibernated is not cleared when a sleeping server is stopped or suspended,
	// so the desired state has to be checked too: otherwise the activator kept
	// listening in front of a server nobody may start, and kept calling wake.
	if r.ActivatorImage == "" || r.WakeURL == "" || s.Hibernation.Proxy || !s.Hibernated || !s.Hibernation.WakeOnConnect ||
		s.DesiredState != models.StateRunning {
		return false
	}
	return hasTCPPort(serverPorts(s, t))
}

// ensureActivator creates the activator Deployment for the active mode, or
// removes it when neither mode applies.
func (r *Reconciler) ensureActivator(ctx context.Context, s *models.Server, t *models.Template, proxy, drop bool) error {
	if !proxy && !drop {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ActivatorName, Namespace: s.Namespace}}
		if err := r.Client.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	dep := BuildActivatorDeployment(s, t, ActivatorParams{
		Image:     r.ActivatorImage,
		WakeURL:   r.WakeURL,
		ActiveURL: r.ActiveURL,
		Token:     crypto.WakeToken(r.WakeKey, s.Slug),
		Proxy:     proxy,
	})
	if activatorTakesNewHelpers(s) {
		return r.applyNewHelpers(ctx, dep, r.ActivatorImage)
	}
	return r.applyKeepingHelpers(ctx, dep, r.ActivatorImage)
}

// ensureInternalService maintains the proxy's stable backend Service.
func (r *Reconciler) ensureInternalService(ctx context.Context, s *models.Server, t *models.Template, proxy bool) error {
	if !proxy || len(serverPorts(s, t)) == 0 {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: InternalServiceName, Namespace: s.Namespace}}
		if err := r.Client.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	return r.apply(ctx, BuildInternalService(s, t))
}

func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, s *models.Server, t *models.Template) error {
	return r.apply(ctx, BuildNetworkPolicy(s, t, r.egressPeersFor(s)))
}

// egressPeersFor lists what this server may reach inside the private address
// space the default policy denies: the managed databases it has been given, the
// servers it was given to reach, plus whatever the operator allowed cluster-wide.
//
// An external database named by DNS cannot be expressed here — a NetworkPolicy
// has no notion of hostnames — so one on a private address needs its range in
// ExtraEgressCIDRs. A public one is already covered by the internet rule.
func (r *Reconciler) egressPeersFor(s *models.Server) []EgressPeer {
	peers := make([]EgressPeer, 0, len(r.ExtraEgressCIDRs)+len(r.DNSServers))
	for _, c := range r.ExtraEgressCIDRs {
		peers = append(peers, EgressPeer{CIDR: c})
	}
	for _, ip := range r.DNSServers {
		if a := net.ParseIP(ip); a != nil {
			bits := 32
			if a.To4() == nil {
				bits = 128
			}
			peers = append(peers, EgressPeer{CIDR: fmt.Sprintf("%s/%d", a, bits), DNS: true})
		}
	}
	if r.Store == nil {
		return peers
	}
	dbs, err := r.Store.ListServerDatabases(s.ID)
	if err != nil {
		// Failing open would hand the server the cluster network; failing closed
		// costs it a database it can reconnect to on the next resync.
		log.Printf("network policy for %s: list databases (its database egress is denied until this clears): %v", s.Slug, err)
		return peers
	}
	seen := map[string]bool{}
	for i := range dbs {
		h, err := r.Store.GetDatabaseHost(dbs[i].HostID)
		if err != nil {
			continue
		}
		switch {
		case h.Kind == models.DBHostManaged:
			if ns := ManagedDBNamespace(h); ns != "" && !seen[ns] {
				seen[ns] = true
				peers = append(peers, EgressPeer{Namespace: ns})
			}
		default:
			for _, p := range r.dbHostPeers(h) {
				if k := fmt.Sprintf("%s|%s|%d", p.Namespace, p.CIDR, p.Port); !seen[k] {
					seen[k] = true
					peers = append(peers, p)
				}
			}
		}
	}
	// The servers it was given to reach, a proxy's backends: their namespaces,
	// where their own policy admits only their game ports. One deleted since,
	// or moved to another cluster, is reached no more.
	for _, slug := range s.Reaches {
		t, err := r.Store.GetServerBySlug(slug)
		if err != nil || t.ClusterID != s.ClusterID || t.Namespace == "" || seen[t.Namespace] {
			continue
		}
		seen[t.Namespace] = true
		peers = append(peers, EgressPeer{Namespace: t.Namespace})
	}
	return peers
}

// Nameservers reads the resolvers of a resolv.conf. On a pod they are the
// cluster's -- the kubelet's clusterDNS, which game servers are handed too.
// Loopback and unspecified addresses, which mean nothing from another pod,
// are left out.
func Nameservers(resolvConf string) []string {
	var out []string
	for _, line := range strings.Split(resolvConf, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if ip := net.ParseIP(f[1]); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			out = append(out, ip.String())
		}
	}
	return out
}

// ensureSecret creates/updates the per-server Secret, skipping the write when
// the stored contents already match. (Secret.stringData is write-only, so we
// compare against the decoded Data; this avoids SSA's stringData pitfalls.)
func (r *Reconciler) ensureSecret(ctx context.Context, want *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if secretDataEqual(existing.Data, want.StringData) {
		return nil
	}
	existing.Labels = mergeLabels(existing.Labels, want.Labels)
	existing.Data = nil
	existing.StringData = want.StringData
	return r.Client.Update(ctx, existing)
}

func secretDataEqual(data map[string][]byte, want map[string]string) bool {
	if len(data) != len(want) {
		return false
	}
	for k, v := range want {
		if string(data[k]) != v {
			return false
		}
	}
	return true
}

// updateStatus reads the workload + pods and writes an observed status to the DB,
// including crash detection.
func (r *Reconciler) updateStatus(ctx context.Context, s *models.Server, t *models.Template) error {
	return r.writeStatus(ctx, s, t, "")
}

// writeStatus is updateStatus with a notice, which says what this pass could
// not put in place and is added to the message.
func (r *Reconciler) writeStatus(ctx context.Context, s *models.Server, t *models.Template, notice string) error {
	eps, addr, portEps := r.endpointsFor(ctx, s, t)
	// A missed done line is remembered while the server is stopped or asleep:
	// it is about the next start.
	st := models.Status{Endpoints: eps, Address: addr, PortEndpoints: portEps,
		InstalledGeneration: s.Status.InstalledGeneration, StartupMissed: s.Status.StartupMissed}

	switch {
	case s.DesiredState == models.StateSuspended:
		st.Phase = models.PhaseSuspended
	case s.DesiredState == models.StateStopped:
		st.Phase = models.PhaseStopped
		st.Message = r.placementProblem(ctx, s, "")
	case s.Hibernated:
		// It will have to wake where its data is.
		st.Phase = models.PhaseHibernated
		st.Message = r.placementProblem(ctx, s, "")
	case s.RestartRequestedAt != nil:
		st.Phase = models.PhaseStopping
		st.Message = "restarting: the game is stopped first, and starts again once it is down"
	default: // Running
		h := r.inspectPods(ctx, s.Namespace, s.Slug)
		st.CrashCount = h.restarts
		switch {
		// The install is checked before readiness: a pod whose init container
		// keeps failing is never ready, and reporting that as "Starting" is what
		// hid the failure. Ready still wins over "installing", since a running
		// pod has by definition finished its init containers.
		case h.installFailed:
			st.Phase = models.PhaseError
			st.Message = installFailureMessage(h)
		case r.deploymentReady(ctx, s.Namespace):
			st.Phase, st.StartedContainer, st.Message, st.StartupMissed = r.startupPhase(s, t, h, time.Now())
			if r.deploymentCurrent(ctx, s.Namespace) {
				st.InstalledGeneration = s.InstallGeneration
			}
		case h.crashloop:
			st.Phase = models.PhaseCrashed
			st.Message = h.msg
		case h.installing:
			st.Phase = models.PhaseInstalling
			st.Message = "running " + h.installStep
		default:
			st.Phase = models.PhaseStarting
			st.Message = r.placementProblem(ctx, s, h.unscheduled)
		}
		r.emitRestartEvents(s, s.Status, h, st.CrashCount)
	}

	// Surface the silent no-op: enabling SFTP needs a system image (the SFTP
	// binary ships in it), otherwise the sidecar is never added and the toggle
	// looks active while nothing serves.
	if s.SFTP.Enabled && r.ActivatorImage == "" {
		st.Message = joinNotice(st.Message, "SFTP is enabled but no system image is configured (set QUETZAL_IMAGE); the SFTP sidecar will not start")
	}
	st.Message = joinNotice(st.Message, notice)

	r.emitTransition(s, s.Status.Phase, st)
	return r.Store.UpdateServerStatus(s.ID, st)
}

// joinNotice adds a notice to a status message.
func joinNotice(msg, notice string) string {
	switch {
	case notice == "":
		return msg
	case msg == "":
		return notice
	}
	return msg + "; " + notice
}

// startupLimit is how long a game has to print its done line. Past it, the
// server is reported Running anyway, with a message: a done line an update of
// the game no longer prints would otherwise keep it Starting for good.
const startupLimit = 30 * time.Minute

// startupPhase tells Running from Starting once the pod is ready. Ready only
// means the container is up; the game is ready when it prints one of its
// template's done lines, which is what Pterodactyl waits for too. It returns
// the phase, the container known to have started, a message, and whether this
// start is going without its done line.
//
// A game whose done line never comes -- Counter-Strike 2 without a valid game
// server token never prints "Connection to Steam servers successful", yet takes
// players -- was shown Starting for the first half hour of every start. Once
// the wait has run out, the next starts are reported Running as soon as their
// container is up, with a message saying why. The line is still looked for, and
// seeing it again brings back the usual wait.
func (r *Reconciler) startupPhase(s *models.Server, t *models.Template, h podHealth, now time.Time) (models.Phase, string, string, bool) {
	lines := t.DoneLines()
	ms, bad := startup.Compile(lines)
	id := h.gameContainer
	switch {
	case len(ms) == 0:
		// Nothing to wait for: the container being up is all there is to know.
		msg := ""
		if bad != nil {
			msg = "the template's startup line is invalid (" + bad.Error() + "), so the server is not checked for it"
		}
		return models.PhaseRunning, id, msg, false
	case r.StartupSeen == nil:
		return models.PhaseRunning, id, "", false
	case id == "" || h.gameStarted.IsZero():
		// The Deployment and the pod list disagree for a moment. Keep what was
		// reported rather than flap between phases.
		if s.Status.Phase == models.PhaseRunning || s.Status.Phase == models.PhaseStarting {
			return s.Status.Phase, s.Status.StartedContainer, s.Status.Message, s.Status.StartupMissed
		}
		return models.PhaseStarting, "", "", s.Status.StartupMissed
	case s.Status.StartedContainer == id && !s.Status.StartupMissed:
		return models.PhaseRunning, id, "", false
	case s.Status.StartedContainer == "" && s.Status.Phase == models.PhaseRunning:
		// Reported Running before this check existed. Sending every running
		// server back to Starting on upgrade, some for good because their done
		// line has since left the log, would be worse than trusting it.
		return models.PhaseRunning, id, "", false
	}
	deadline := h.gameStarted.Add(startupLimit)
	if r.StartupSeen(s.Namespace, h.gamePod, id, deadline, ms) {
		return models.PhaseRunning, id, "", false
	}
	switch {
	case !now.Before(deadline):
		return models.PhaseRunning, id, fmt.Sprintf("the console never showed %s within %s of starting; the template's startup line may be out of date",
			quoteLines(lines), minutes(startupLimit)), true
	case s.Status.StartupMissed:
		return models.PhaseRunning, id, fmt.Sprintf("the console did not show %s when this server last started, so it is not waited for this time; the template's startup line may be out of date",
			quoteLines(lines)), true
	}
	return models.PhaseStarting, "", fmt.Sprintf("waiting for %s in the console, for up to %s", quoteLines(lines), minutes(startupLimit)), false
}

// minutes writes a whole number of minutes for a message: "30 minutes".
func minutes(d time.Duration) string {
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// quoteLines lists done lines for a message: "a" or "b".
func quoteLines(lines []string) string {
	q := make([]string, len(lines))
	for i, l := range lines {
		q[i] = fmt.Sprintf("%q", l)
	}
	return strings.Join(q, " or ")
}

// installFailureMessage says which step failed, with what code, and quotes what
// the step wrote if Kubernetes captured it. The point is that the message alone
// is enough to act on: "install exited with code 7" sends someone to the egg's
// script, not to a support forum.
func installFailureMessage(h podHealth) string {
	msg := fmt.Sprintf("%s failed (exit code %d)", h.installStep, h.installExit)
	if h.installMessage != "" {
		msg += ": " + h.installMessage
	}
	return msg + " — see the install log for the output"
}

// emitTransition records an event when a server crosses into a phase worth
// notifying about. It only covers transitions the API can't already see
// (the controller observes crashes, readiness and idle-hibernation); power
// actions are emitted by the API itself, so they are not duplicated here.
func (r *Reconciler) emitTransition(s *models.Server, old models.Phase, st models.Status) {
	if old == st.Phase {
		return
	}
	switch st.Phase {
	case models.PhaseRunning:
		r.emitEvent(s, models.EventServerRunning, "is up and running")
	case models.PhaseError:
		// Reaching Error means the install or the config render gave up. Say so
		// once, on the transition, so a notification channel is told rather than
		// leaving it to whoever next opens the panel.
		r.emitEvent(s, models.EventServerInstallFailed, st.Message)
	case models.PhaseCrashed:
		msg := "crashed"
		if st.CrashCount > 0 {
			msg = fmt.Sprintf("crashed (%d restarts)", st.CrashCount)
		}
		if st.Message != "" {
			msg += ": " + st.Message
		}
		r.emitEvent(s, models.EventServerCrashed, msg)
	case models.PhaseHibernated:
		r.emitEvent(s, models.EventServerHibernated, "hibernated after inactivity")
	case models.PhaseStopped:
		// The panel offered server.stopped as a filter and nothing recorded it.
		// A server that was never started reads Stopped from the outset, which
		// is not news: only one that was up, or on its way, goes down.
		switch old {
		case models.PhaseRunning, models.PhaseStarting, models.PhaseStopping, models.PhaseCrashed, models.PhaseHibernated:
			r.emitEvent(s, models.EventServerStopped, "is stopped")
		}
	}
}

// emitRestartEvents records an event when a container restart is newly observed,
// so a server that keeps dying and coming back (classically an OOM loop) is
// visible in the activity log instead of restarting silently. It fires on any
// growth in the cumulative restart count, and also when the count resets to a
// positive value (a fresh pod that already restarted before we first saw it).
func (r *Reconciler) emitRestartEvents(s *models.Server, old models.Status, h podHealth, newCount int) {
	increased := newCount > old.CrashCount
	reset := newCount < old.CrashCount && newCount > 0
	if !increased && !reset {
		return
	}
	switch {
	case h.oomKilled:
		r.emitEvent(s, models.EventServerOOMKilled,
			fmt.Sprintf("ran out of memory (OOMKilled) and was restarted — %d restart(s) so far", newCount))
	case h.crashloop:
		// The crashloop phase transition already emits server.crashed; don't double up.
	case h.exitCode != 0:
		r.emitEvent(s, models.EventServerRestarted,
			fmt.Sprintf("container exited (code %d) and was restarted — %d so far", h.exitCode, newCount))
	default:
		r.emitEvent(s, models.EventServerRestarted,
			fmt.Sprintf("container restarted — %d so far", newCount))
	}
}

// emitEvent appends a server-scoped event (best-effort). The apiserver's
// dispatcher delivers it on its next pass.
func (r *Reconciler) emitEvent(s *models.Server, eventType, message string) {
	_ = r.Store.AddEvent(&models.Event{
		ServerID: s.ID, Type: eventType, Message: s.Slug + ": " + message,
	})
}

func (r *Reconciler) deploymentReady(ctx context.Context, ns string) bool {
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, dep); err != nil {
		return false
	}
	return dep.Status.ReadyReplicas >= 1
}

// deploymentCurrent reports whether the Deployment's ready pod runs its current
// revision: the controller has observed the latest spec and a pod from it is
// ready. A spec applied a moment ago is not current yet, so a pod that predates
// a reinstall is never taken for one that went through it.
func (r *Reconciler) deploymentCurrent(ctx context.Context, ns string) bool {
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, dep); err != nil {
		return false
	}
	return dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas >= 1 && dep.Status.ReadyReplicas >= 1
}

func (r *Reconciler) deploymentRunning(ctx context.Context, ns string) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Spec.Replicas != nil && *dep.Spec.Replicas > 0, nil
}

// podHealth is what inspectPods observes about a server's pods: cumulative
// container restarts, whether any container is in CrashLoopBackOff, and why the
// last restart happened (so a silent OOM restart loop can be surfaced).
type podHealth struct {
	restarts   int
	crashloop  bool
	msg        string
	oomKilled  bool
	termReason string // last termination reason, e.g. "OOMKilled", "Error"
	exitCode   int32  // last termination exit code (0 when unknown)

	// The init containers — the egg's install step and the config render — are
	// the part nothing used to look at. A failure there never reaches
	// ContainerStatuses (the main container has not started), so the server sat
	// on "Starting" for good: no phase, no message, no event, while the answer
	// was in the init container's log the whole time.
	installing     bool
	installFailed  bool
	installStep    string // the init container concerned
	installExit    int32
	installMessage string

	// The game container that is up, if any: its pod, its runtime ID (new on
	// every restart) and when it started.
	gamePod       string
	gameContainer string
	gameStarted   time.Time

	// unscheduled is why no node can take the game pod, while it cannot.
	unscheduled string
}

// inspectPods sums container restarts, detects CrashLoopBackOff, and records the
// most recent termination (reason + exit code) — including OOMKilled, which
// otherwise leaves no trace when the container restarts fast enough to never
// enter CrashLoopBackOff.
func (r *Reconciler) inspectPods(ctx context.Context, ns, slug string) podHealth {
	var h podHealth
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{serverLabel: slug}); err != nil {
		return h
	}
	note := func(term *corev1.ContainerStateTerminated) {
		if term == nil {
			return
		}
		h.termReason = term.Reason
		h.exitCode = term.ExitCode
		if term.Reason == "OOMKilled" {
			h.oomKilled = true
		}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		// A pod on its way out (a stop, a restart, a new spec) or done for
		// (evicted) had its game killed, which is no crash.
		current := p.DeletionTimestamp == nil && p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded
		if why := Unschedulable(p); current && why != "" {
			h.unscheduled = why
		}
		for _, cs := range p.Status.ContainerStatuses {
			h.restarts += int(cs.RestartCount)
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				h.crashloop = true
				h.msg = r.crashMessageWithLog(ctx, ns, p.Name, cs)
			}
			// Down after a failed run is a crash too, whatever the kubelet calls
			// the wait before the next one. Kubernetes 1.35 reports the failed
			// run as terminated through the short back-offs and says
			// CrashLoopBackOff only once they reach minutes, so a game failing
			// at every start stayed "Starting" for five minutes.
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && current {
				h.crashloop = true
				h.msg = r.crashMessageWithLog(ctx, ns, p.Name, cs)
			}
			// The previous run's exit explains a restart even once the container is
			// back up; a container terminated right now is captured too.
			note(cs.LastTerminationState.Terminated)
			note(cs.State.Terminated)
			if run := cs.State.Running; run != nil && cs.Name == workloadName && cs.ContainerID != "" &&
				pods.Items[i].DeletionTimestamp == nil && !run.StartedAt.Time.Before(h.gameStarted) {
				h.gamePod, h.gameContainer, h.gameStarted = pods.Items[i].Name, cs.ContainerID, run.StartedAt.Time
			}
		}
		noteInit(&h, pods.Items[i].Status.InitContainerStatuses)
	}
	return h
}

// crashMessageWithLog is crashMessage with the last error line of the run's
// log, when LogTail can read it. How a run ended is not always why: Paper out
// of heap prints java.lang.OutOfMemoryError and exits 0, and its server read
// "the game exited with code 0", which sent nobody towards the memory.
func (r *Reconciler) crashMessageWithLog(ctx context.Context, ns, pod string, cs corev1.ContainerStatus) string {
	msg := crashMessage(cs)
	if r.LogTail == nil || cs.Name != workloadName {
		return msg
	}
	// The run that ended is the current container while it is down, the
	// previous one once the kubelet has started another.
	previous := cs.State.Terminated == nil
	tail, err := r.LogTail(ctx, ns, pod, cs.Name, previous, 50)
	if err != nil {
		return msg
	}
	line := lastErrorLine(tail)
	switch {
	case line == "":
		return msg
	case strings.Contains(line, "OutOfMemoryError"):
		return msg + ", out of memory: " + line + " — give it more memory"
	}
	return msg + "; its log ends with: " + line
}

// errorLine is what a line that says why a game stopped tends to hold.
var errorLine = regexp.MustCompile(`(?i)(exception|error|fatal|panic|out of memory|segmentation fault|killed)`)

// lastErrorLine is the last line of a log that reads as an error, cut short.
func lastErrorLine(log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(ansi.ReplaceAllString(lines[i], ""))
		if errorLine.MatchString(l) {
			if len(l) > 200 {
				l = l[:200] + "…"
			}
			return l
		}
	}
	return ""
}

// ansi matches the colour codes a game's log is full of.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// crashMessage says how the game's last run ended. The kubelet's own message
// ("back-off 2m40s restarting failed container=server pod=...") said neither
// how nor why.
func crashMessage(cs corev1.ContainerStatus) string {
	t := cs.State.Terminated
	if t == nil {
		t = cs.LastTerminationState.Terminated
	}
	switch {
	case t != nil && t.Reason == "OOMKilled":
		return "the game ran out of memory (OOMKilled)"
	case t != nil:
		return fmt.Sprintf("the game exited with code %d", t.ExitCode)
	case cs.State.Waiting != nil && cs.State.Waiting.Message != "":
		return cs.State.Waiting.Message
	}
	return "container in CrashLoopBackOff"
}

// noteInit reads the init containers: the install step and the config render.
// Exit 0 means done, and a pod that has started its main container reports them
// all that way — so only a non-zero exit, or a back-off after one, is a failure.
// Anything still pending means the install is in progress, which is worth saying
// too: a large modpack takes minutes and used to be indistinguishable from a
// server that simply would not start.
func noteInit(h *podHealth, statuses []corev1.ContainerStatus) {
	for _, cs := range statuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
			h.installFailed, h.installStep = true, cs.Name
			h.installExit = cs.State.Terminated.ExitCode
			h.installMessage = strings.TrimSpace(cs.State.Terminated.Message)
		case cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff":
			// Kubernetes retries a failed init container in place; while it waits
			// between attempts the exit code is only on the previous state.
			h.installFailed, h.installStep = true, cs.Name
			if t := cs.LastTerminationState.Terminated; t != nil {
				h.installExit = t.ExitCode
				h.installMessage = strings.TrimSpace(t.Message)
			}
		case cs.State.Terminated == nil:
			h.installing = true
			if h.installStep == "" {
				h.installStep = cs.Name
			}
		}
	}
}

// endpointsFor computes the reachable addresses for a server and picks a primary
// one (the primary port, or the sole port). External exposure (NodePort/
// LoadBalancer) yields node/LB addresses; otherwise the in-cluster DNS names.
func (r *Reconciler) endpointsFor(ctx context.Context, s *models.Server, t *models.Template) (eps []string, addr string, portEps []models.PortEndpoint) {
	ports := serverPorts(s, t)
	add := func(p models.PortSpec, ep string) {
		eps = append(eps, ep)
		primary := p.Primary || len(ports) == 1
		if addr == "" && primary {
			addr = ep
		}
		portEps = append(portEps, models.PortEndpoint{
			Port: p.Port, Protocol: string(protocol(p.Protocol)), Address: ep, Primary: primary,
		})
	}

	switch s.Expose.ServiceType() {
	case models.ExposeNodePort:
		host := r.endpointHost(ctx, s)
		if host == "" {
			host = "<node-ip>"
		}
		for _, p := range ports {
			if p.NodePort == 0 {
				continue
			}
			add(p, net.JoinHostPort(host, strconv.Itoa(int(p.NodePort))))
		}
	case models.ExposeLoadBalancer:
		host := r.loadBalancerAddress(ctx, s.Namespace)
		if host == "" {
			break // not yet provisioned
		}
		for _, p := range ports {
			add(p, net.JoinHostPort(host, strconv.Itoa(int(p.Port))))
		}
	default: // ClusterIP
		for _, p := range ports {
			add(p, fmt.Sprintf("%s.%s.svc.cluster.local:%d", workloadName, s.Namespace, p.Port))
		}
	}
	if addr == "" && len(eps) > 0 {
		addr = eps[0]
	}
	return eps, addr, portEps
}

// endpointHost is the host published in a server's external NodePort endpoints:
// this cluster's own hostname if it has one, else the panel-wide setting, else
// a node address. Letting the admin pin a hostname means players see a stable,
// memorable address instead of the raw node IP.
//
// Which node is not indifferent when the Service keeps the player's address
// (externalTrafficPolicy: Local, the default): only a node running one of its
// pods answers. The first node of the list was published, and on a cluster of
// several the address shown to players often led nowhere. The game and the
// activator run on the data-manager's node, so that is the one.
func (r *Reconciler) endpointHost(ctx context.Context, s *models.Server) string {
	if h := store.EndpointHostFor(r.Store, r.ClusterID); h != "" {
		return h
	}
	if localTraffic(s) {
		if a := r.dataNodeAddress(ctx, s); a != "" {
			return a
		}
	}
	return r.firstNodeAddress(ctx)
}

// dataNodeAddress is the address of the node the server's data-manager runs
// on, or "" while it has none (not yet scheduled, or scaled down for a restore).
func (r *Reconciler) dataNodeAddress(ctx context.Context, s *models.Server) string {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels{DataLabel: s.Slug}); err != nil {
		return ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.DeletionTimestamp != nil {
			continue
		}
		var node corev1.Node
		if err := r.Client.Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil {
			return ""
		}
		return NodeAddress([]corev1.Node{node})
	}
	return ""
}

// firstNodeAddress returns a usable node address, preferring an ExternalIP and
// falling back to an InternalIP.
func (r *Reconciler) firstNodeAddress(ctx context.Context) string {
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return ""
	}
	return NodeAddress(nodes.Items)
}

// NodeAddress picks the address to reach a cluster on: the first ExternalIP,
// falling back to the first InternalIP, and "" when neither exists. Exported so
// the apiserver derives the same address as the controller from its own client,
// instead of keeping a second copy of the preference order.
func NodeAddress(nodes []corev1.Node) string {
	var internal string
	for i := range nodes {
		for _, a := range nodes[i].Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				if a.Address != "" {
					return a.Address
				}
			case corev1.NodeInternalIP:
				if internal == "" {
					internal = a.Address
				}
			}
		}
	}
	return internal
}

// loadBalancerAddress returns the Service's external LB address once assigned.
func (r *Reconciler) loadBalancerAddress(ctx context.Context, ns string) string {
	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workloadName}, svc); err != nil {
		return ""
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP
		}
		if ing.Hostname != "" {
			return ing.Hostname
		}
	}
	return ""
}

func mergeLabels(into, from map[string]string) map[string]string {
	if into == nil {
		into = map[string]string{}
	}
	for k, v := range from {
		into[k] = v
	}
	return into
}

// isConsoleStop reports whether a template's stop command should be written to
// the container's stdin for a graceful stop. Pterodactyl encodes a signal-based
// stop as a caret token (e.g. "^C" = SIGINT, used by some proxies/limbos); that
// isn't console input, so writing the literal "^C" does nothing. For those we
// skip the stdin write and let pod termination deliver SIGTERM (+ the grace
// period), which those servers handle as a clean shutdown.
func isConsoleStop(cmd string) bool {
	return cmd != "" && !strings.HasPrefix(cmd, "^")
}
