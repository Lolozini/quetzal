package cluster

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/models"
)

// The manifest handed to an operator registering a remote cluster grants the
// same access the chart grants locally. Two copies of a permission set drift,
// and drift here is silent in both directions: too little and the feature fails
// on remote clusters only, too much and every operator who follows the
// instructions grants more than Quetzal needs.
//
// Leader election is the one deliberate difference — it happens only where the
// control plane runs — so it lives in a namespaced Role in the chart and has no
// place in the remote manifest.
func TestRemoteRulesMatchTheChart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "quetzal", "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read chart: %v", err)
	}
	chartRules := func(suffix string) []PolicyRule {
		t.Helper()
		for _, doc := range strings.Split(string(raw), "\n---") {
			if !regexp.MustCompile(`(?m)^kind: ClusterRole$`).MatchString(doc) {
				continue
			}
			if !strings.Contains(doc, `name: {{ include "quetzal.fullname" . }}`+suffix) {
				continue
			}
			i := strings.Index(doc, "\nrules:")
			if i < 0 {
				t.Fatalf("ClusterRole%s has no rules", suffix)
			}
			var parsed struct {
				Rules []PolicyRule `yaml:"rules"`
			}
			// resourceNames carries a Helm expression; the drift check is about
			// which permissions exist, not how the name is rendered.
			body := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(doc[i:], "TEMPLATED")
			if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("parse rules%s: %v", suffix, err)
			}
			return parsed.Rules
		}
		t.Fatalf("no ClusterRole%s in the chart", suffix)
		return nil
	}
	compare := func(what string, chart, manifest []PolicyRule) {
		t.Helper()
		key := func(r PolicyRule) string {
			return strings.Join(r.APIGroups, ",") + "|" + strings.Join(r.Resources, ",") + "|" + strings.Join(r.Verbs, ",")
		}
		inChart := map[string]bool{}
		for _, r := range chart {
			inChart[key(r)] = true
		}
		inManifest := map[string]bool{}
		for _, r := range manifest {
			inManifest[key(r)] = true
		}
		for k := range inManifest {
			if !inChart[k] {
				t.Errorf("%s: the remote manifest grants a rule the chart does not: %s", what, k)
			}
		}
		for k := range inChart {
			if !inManifest[k] {
				t.Errorf("%s: the chart grants a rule the remote manifest does not, so the feature it serves fails on remote clusters: %s", what, k)
			}
		}
	}
	compare("cluster-scoped", chartRules("-cluster"), RemoteClusterRules)
	compare("namespaced", chartRules("-namespaced"), RemoteNamespacedRules)
}

// The manifest is applied by hand on someone else's cluster, so it has to parse
// and it has to create the account it claims to.
func TestRemoteManifestIsValidYAML(t *testing.T) {
	docs := strings.Split(RemoteManifest(), "\n---\n")
	if len(docs) != 8 {
		t.Fatalf("got %d documents, want namespace, account, two roles, binding, token and the admission policy pair", len(docs))
	}
	kinds := map[string]bool{}
	for _, d := range docs {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(d), &obj); err != nil {
			t.Fatalf("document does not parse: %v\n%s", err, d)
		}
		kinds[obj.Kind] = true
		if obj.Metadata.Namespace != "" && obj.Metadata.Namespace != RemoteNamespace {
			t.Errorf("%s lands in %q, not Quetzal's own namespace", obj.Kind, obj.Metadata.Namespace)
		}
	}
	for _, want := range []string{
		"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Secret",
		"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding",
	} {
		if !kinds[want] {
			t.Errorf("manifest has no %s", want)
		}
	}
	// Nothing here should be cluster-admin by another name.
	if strings.Contains(RemoteManifest(), `"*"`) {
		t.Error("the manifest grants a wildcard")
	}
	if !strings.Contains(RemoteManifest(), "kubernetes.io/service-account-token") {
		t.Error("no long-lived token: a projected one expires while Quetzal is still using it")
	}
	// The namespaced role must not be bound across the cluster — that is the
	// whole point of splitting it out.
	for _, d := range docs {
		if strings.Contains(d, "kind: ClusterRoleBinding") && strings.Contains(d, RemoteServiceAccount+"-namespaced") {
			t.Error("the namespaced role is bound cluster-wide, which undoes the split")
		}
	}
}

// The control plane binds a role to itself in each namespace it creates. On a
// remote cluster that has to be the role the manifest creates there, and one
// its account may bind. 0.5.0 bound the chart's role on every cluster; a remote
// one refused ("attempting to grant RBAC permissions not currently held"), and
// no server ran on a cluster registered as documented.
func TestRemoteClustersBindTheManifestRole(t *testing.T) {
	type doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Rules   []PolicyRule `yaml:"rules"`
		RoleRef struct {
			Name string `yaml:"name"`
		} `yaml:"roleRef"`
	}
	roles := map[string][]PolicyRule{}
	var boundClusterWide []string
	dec := yaml.NewDecoder(strings.NewReader(RemoteManifest()))
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse manifest: %v", err)
		}
		switch d.Kind {
		case "ClusterRole":
			roles[d.Metadata.Name] = d.Rules
		case "ClusterRoleBinding":
			boundClusterWide = append(boundClusterWide, d.RoleRef.Name)
		}
	}

	remote := NamespacedRole(&models.Cluster{Slug: "remote"}, "quetzal-namespaced")
	if _, ok := roles[remote]; !ok {
		t.Fatalf("a remote cluster binds %q, which the manifest does not create", remote)
	}
	if slices.Contains(boundClusterWide, remote) {
		t.Errorf("the manifest binds %q across the cluster; it is meant for the namespaces Quetzal creates only", remote)
	}
	mayBind := false
	for _, name := range boundClusterWide {
		for _, r := range roles[name] {
			if slices.Contains(r.Verbs, "bind") && slices.Contains(r.Resources, "clusterroles") && slices.Contains(r.ResourceNames, remote) {
				mayBind = true
			}
		}
	}
	if !mayBind {
		t.Errorf("the manifest's account may not bind %q", remote)
	}

	if got := NamespacedRole(&models.Cluster{InCluster: true}, "quetzal-namespaced"); got != "quetzal-namespaced" {
		t.Errorf("the local cluster binds %q, want the chart's role", got)
	}
}

// The script takes the API server's address from the operator's own context,
// which is often an address of their machine alone. It says so, on stderr so
// that the kubeconfig it prints stays clean.
func TestKubeconfigScriptWarnsAboutALoopbackAddress(t *testing.T) {
	bin := t.TempDir()
	kubectl := "#!/bin/sh\ncase \"$*\" in\n  *\"config view\"*) printf '%s' \"$FAKE_SERVER\" ;;\n  *) printf 'Y2E=' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	for server, warn := range map[string]bool{
		"https://127.0.0.1:6443":            true,
		"https://127.0.0.1:41245":           true,
		"https://localhost:6443":            true,
		"https://[::1]:6443":                true,
		"https://10.0.0.5:6443":             false,
		"https://api.example.com:6443":      false,
		"https://localhost.example.com:443": false,
	} {
		cmd := exec.Command("sh", "-c", RemoteKubeconfigScript())
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_SERVER="+server)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v (%s)", server, err, stderr.String())
		}
		if got := strings.Contains(stderr.String(), "warning"); got != warn {
			t.Errorf("%s: warned=%v, want %v (stderr %q)", server, got, warn, stderr.String())
		}
		if !strings.Contains(stdout.String(), "server: "+server+"\n") || strings.Contains(stdout.String(), "warning") {
			t.Errorf("%s: the kubeconfig is not clean:\n%s", server, stdout.String())
		}
	}
}

// A cluster registered at a loopback address cannot be reached from Quetzal,
// whatever works on the operator's machine. The failure says why.
func TestProbeExplainsALoopbackAddress(t *testing.T) {
	failed := errors.New("dial tcp: connection refused")
	for host, hint := range map[string]bool{
		"https://127.0.0.1:6443":  true,
		"127.0.0.1:41245":         true,
		"https://localhost:6443":  true,
		"https://[::1]:6443":      true,
		"https://10.96.0.1:443":   false,
		"https://k8s.example.com": false,
	} {
		err := loopbackHint(&rest.Config{Host: host}, failed)
		if !errors.Is(err, failed) {
			t.Errorf("%s: the original error was lost: %v", host, err)
		}
		if got := strings.Contains(err.Error(), "loopback"); got != hint {
			t.Errorf("%s: hint=%v, want %v (%v)", host, got, hint, err)
		}
	}
}
