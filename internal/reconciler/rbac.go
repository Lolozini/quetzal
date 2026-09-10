package reconciler

import (
	"context"
	"log"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleBindingName is the binding the control plane gives itself in each
// namespace it creates.
const RoleBindingName = "quetzal"

// ensureRoleBinding grants the control plane, inside this one namespace, the
// access it needs to run a server there.
//
// The alternative is what Quetzal used to do: hold that access across the whole
// cluster, which means reading every Secret on it — including the tokens in
// kube-system. RBAC cannot say "namespaces beginning with quetzal-", so the
// grant has to be made per namespace, by the thing that creates them.
//
// Best-effort on purpose. On a cluster registered with an administrator's
// kubeconfig there is no role of ours to bind and none is needed, so a failure
// here must not stop the reconcile — the steps that follow will say plainly
// enough if the access really is missing.
func (r *Reconciler) ensureRoleBinding(ctx context.Context, namespace string) {
	if r.NamespacedRole == "" {
		return // not configured (tests, or an install that predates the split)
	}
	subject, ok := r.identity(ctx)
	if !ok {
		return
	}
	rb := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      RoleBindingName,
			Namespace: namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.NamespacedRole,
		},
		Subjects: []rbacv1.Subject{subject},
	}
	err := r.Client.Create(ctx, rb)
	if err == nil || apierrors.IsAlreadyExists(err) {
		return
	}
	r.warnOnce("role-binding", "cannot grant myself access in %s (%v); this is expected when the cluster was registered with an administrator's kubeconfig, and a problem otherwise", namespace, err)
}

// identity asks the cluster who this control plane authenticates as, so the
// binding names the right subject. A kubeconfig can carry anything — a service
// account on that cluster, a client certificate, an OIDC user — and only the
// cluster knows which.
//
// Resolved once per Reconciler: it is one call, and the answer cannot change
// without new credentials, which build a new client anyway.
func (r *Reconciler) identity(ctx context.Context) (rbacv1.Subject, bool) {
	r.identityMu.Lock()
	defer r.identityMu.Unlock()
	if r.identityDone {
		return r.identitySubject, r.identityOK
	}
	r.identityDone = true

	review := &authnv1.SelfSubjectReview{}
	if err := r.Client.Create(ctx, review); err != nil {
		log.Printf("cannot determine who I am on this cluster (%v); per-namespace access will not be granted", err)
		return rbacv1.Subject{}, false
	}
	name := review.Status.UserInfo.Username
	if ns, sa, isSA := serviceAccountParts(name); isSA {
		r.identitySubject = rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: ns}
	} else if name != "" {
		r.identitySubject = rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}
	} else {
		return rbacv1.Subject{}, false
	}
	r.identityOK = true
	return r.identitySubject, true
}

// serviceAccountParts splits "system:serviceaccount:<namespace>:<name>".
func serviceAccountParts(username string) (namespace, name string, ok bool) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(username, prefix)
	ns, sa, found := strings.Cut(rest, ":")
	if !found || ns == "" || sa == "" {
		return "", "", false
	}
	return ns, sa, true
}
