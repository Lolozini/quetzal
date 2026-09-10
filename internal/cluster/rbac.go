package cluster

import (
	"fmt"
	"strings"
)

// Registering a remote cluster means handing Quetzal a kubeconfig, and the
// easiest kubeconfig to hand over is the admin one. That gives the control plane
// — and anyone who reaches it — everything on that cluster, when what it needs is
// a bounded set. So Quetzal ships the manifest that creates exactly that account,
// and offers it where a cluster is registered rather than burying it in a page
// nobody reads at that moment.
//
// The rules below are the same ones the Helm chart grants on the cluster Quetzal
// runs on, less leader election, which happens only where the control plane
// lives. A test holds the two in step.

// RemoteNamespace is where the manifest puts the account it creates. It is
// Quetzal's own namespace on that cluster, not one that already exists.
const RemoteNamespace = "quetzal-system"

// RemoteServiceAccount is the account the generated kubeconfig authenticates as.
const RemoteServiceAccount = "quetzal-remote"

// PolicyRule is one RBAC rule, in the shape the manifest and the drift test both
// read. It deliberately mirrors rbacv1.PolicyRule without depending on it: this
// is rendered as text, never applied through the API.
type PolicyRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// RemoteRules is what Quetzal needs on a cluster it manages remotely. Every rule
// is here because something calls it; see the chart for the same set on the
// local cluster.
var RemoteRules = []PolicyRule{
	// Per-server namespaces and the workloads inside them. update/patch keep an
	// existing namespace's labels current; create/delete are the lifecycle.
	{APIGroups: []string{""}, Resources: []string{"namespaces"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{""}, Resources: []string{"services", "persistentvolumeclaims", "secrets", "configmaps", "resourcequotas"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	// Cluster-scoped volumes: switching a reclaim policy to Retain when a server
	// is deleted with "keep data".
	{APIGroups: []string{""}, Resources: []string{"persistentvolumes"},
		Verbs: []string{"get", "list", "patch"}},
	{APIGroups: []string{"apps"}, Resources: []string{"deployments"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	// Backups, restores and snapshot deletion run as one-shot Jobs.
	{APIGroups: []string{"batch"}, Resources: []string{"jobs"},
		Verbs: []string{"get", "list", "watch", "create", "delete"}},
	{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	// No create: nothing runs a bare pod. The deletes restart a server and clear
	// a finished operation.
	{APIGroups: []string{""}, Resources: []string{"pods"},
		Verbs: []string{"get", "list", "watch", "delete", "deletecollection"}},
	{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
	// The console and the file manager.
	{APIGroups: []string{""}, Resources: []string{"pods/attach", "pods/exec"}, Verbs: []string{"create", "get"}},
	// Publishing a server's address, and the node picker.
	{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
	{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: []string{"get", "list", "watch"}},
}

// RemoteManifest renders the YAML an operator applies on a cluster they want to
// register: a namespace, a service account bound to the rules above, and a
// long-lived token for it (Kubernetes stopped minting those automatically in
// 1.24, so the Secret is explicit).
func RemoteManifest() string {
	var b strings.Builder
	b.WriteString(`# Run this on the cluster you are registering, with an account that can
# create RBAC — your own kubeconfig, once. It creates a service account for
# Quetzal with the permissions it needs and nothing more, so you never have to
# hand over a cluster-admin kubeconfig.
apiVersion: v1
kind: Namespace
metadata:
  name: ` + RemoteNamespace + `
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ` + RemoteServiceAccount + `
  namespace: ` + RemoteNamespace + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + RemoteServiceAccount + `
rules:
`)
	for _, r := range RemoteRules {
		b.WriteString(fmt.Sprintf("  - apiGroups: [%s]\n    resources: [%s]\n    verbs: [%s]\n",
			quoteList(r.APIGroups), quoteList(r.Resources), quoteList(r.Verbs)))
	}
	b.WriteString(`---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + RemoteServiceAccount + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ` + RemoteServiceAccount + `
subjects:
  - kind: ServiceAccount
    name: ` + RemoteServiceAccount + `
    namespace: ` + RemoteNamespace + `
---
# A token that does not expire. Kubernetes 1.24 stopped creating these on its
# own, and a projected token would expire while Quetzal is still using it.
apiVersion: v1
kind: Secret
metadata:
  name: ` + RemoteServiceAccount + `-token
  namespace: ` + RemoteNamespace + `
  annotations:
    kubernetes.io/service-account.name: ` + RemoteServiceAccount + `
type: kubernetes.io/service-account-token
`)
	return b.String()
}

// RemoteKubeconfigScript prints the kubeconfig to paste back into Quetzal. It
// reads the cluster's own address and CA from the current context, so it works
// wherever the operator already has access.
func RemoteKubeconfigScript() string {
	return `# Then, still pointed at that cluster, print the kubeconfig to paste into
# Quetzal. Everything here comes from your current context.
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl -n ` + RemoteNamespace + ` get secret ` + RemoteServiceAccount + `-token -o jsonpath='{.data.ca\.crt}')
TOKEN=$(kubectl -n ` + RemoteNamespace + ` get secret ` + RemoteServiceAccount + `-token -o jsonpath='{.data.token}' | base64 -d)

cat <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster:
      server: $SERVER
      certificate-authority-data: $CA
users:
  - name: ` + RemoteServiceAccount + `
    user:
      token: $TOKEN
contexts:
  - name: remote
    context:
      cluster: remote
      user: ` + RemoteServiceAccount + `
current-context: remote
EOF
`
}

func quoteList(in []string) string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = `"` + v + `"`
	}
	return strings.Join(out, ", ")
}
