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
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	ResourceNames []string `yaml:"resourceNames,omitempty"`
	Verbs         []string `yaml:"verbs"`
}

// RemoteClusterRules are the permissions that are genuinely cluster-wide,
// because their resources are: namespaces themselves, nodes, volumes, storage
// classes. It also carries the right to hand the namespaced role out, scoped by
// name to that one role.
var RemoteClusterRules = []PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"namespaces"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{""}, Resources: []string{"persistentvolumes"},
		Verbs: []string{"get", "list", "patch"}},
	{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
	{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: []string{"get", "list", "watch"}},
	{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"},
		ResourceNames: []string{RemoteServiceAccount + "-namespaced"}, Verbs: []string{"bind"}},
	{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"selfsubjectreviews"}, Verbs: []string{"create"}},
}

// RemoteNamespacedRules are everything a server needs, granted only inside the
// namespaces Quetzal creates — never across the cluster. This is what keeps a
// compromised control plane away from every other Secret on the cluster.
var RemoteNamespacedRules = []PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"services", "persistentvolumeclaims", "secrets", "configmaps", "resourcequotas"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{"apps"}, Resources: []string{"deployments"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{"batch"}, Resources: []string{"jobs"},
		Verbs: []string{"get", "list", "watch", "create", "delete"}},
	{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"},
		Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	{APIGroups: []string{""}, Resources: []string{"pods"},
		Verbs: []string{"get", "list", "watch", "delete", "deletecollection"}},
	{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
	{APIGroups: []string{""}, Resources: []string{"pods/attach", "pods/exec"}, Verbs: []string{"create", "get"}},
}

// RemoteManifest renders the YAML an operator applies on a cluster they want to
// register: a namespace, a service account, the two roles, and a long-lived
// token (Kubernetes stopped minting those automatically in 1.24, so the Secret
// is explicit).
//
// Only the cluster-scoped role is bound here. The namespaced one is bound by
// the control plane in each namespace it creates, which is what keeps it away
// from every other Secret on the cluster.
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
# Cluster-wide, because these resources are: namespaces themselves, nodes,
# volumes, storage classes — plus the right to hand out the role below, scoped
# by name to that one role.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + RemoteServiceAccount + `
rules:
`)
	writeRules(&b, RemoteClusterRules)
	b.WriteString(`---
# Everything a server needs, and deliberately NOT bound across the cluster.
# Quetzal binds it in each namespace it creates, so reading a Secret anywhere
# else — kube-system included — is not something this account can do.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + RemoteServiceAccount + `-namespaced
rules:
`)
	writeRules(&b, RemoteNamespacedRules)
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
---
# The right to hand out the namespaced role is cluster-wide or nothing in RBAC,
# so on its own it lets this account bind that role in kube-system and read
# everything after all. Admission can say where, so it does. Delete these two if
# your cluster is older than 1.30: everything else still works, you lose this
# guard.
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: ` + RemoteServiceAccount + `-namespace-scope
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["rbac.authorization.k8s.io"]
        apiVersions: ["v1"]
        operations: ["CREATE", "UPDATE"]
        resources: ["rolebindings"]
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["DELETE"]
        resources: ["namespaces"]
  matchConditions:
    - name: only-the-control-plane
      expression: request.userInfo.username == 'system:serviceaccount:` + RemoteNamespace + `:` + RemoteServiceAccount + `'
  variables:
    - name: target
      expression: "request.resource.resource == 'namespaces' ? request.name : request.namespace"
    - name: mine
      expression: "variables.target.startsWith('quetzal-srv-') || variables.target.startsWith('quetzal-db-')"
  validations:
    - expression: variables.mine
      messageExpression: "'Quetzal may only do this in the namespaces it creates, not ' + variables.target"
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: ` + RemoteServiceAccount + `-namespace-scope
spec:
  policyName: ` + RemoteServiceAccount + `-namespace-scope
  validationActions: [Deny]
`)
	return b.String()
}

func writeRules(b *strings.Builder, rules []PolicyRule) {
	for _, r := range rules {
		b.WriteString(fmt.Sprintf("  - apiGroups: [%s]\n    resources: [%s]\n", quoteList(r.APIGroups), quoteList(r.Resources)))
		if len(r.ResourceNames) > 0 {
			b.WriteString(fmt.Sprintf("    resourceNames: [%s]\n", quoteList(r.ResourceNames)))
		}
		b.WriteString(fmt.Sprintf("    verbs: [%s]\n", quoteList(r.Verbs)))
	}
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
