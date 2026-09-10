package cluster

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
