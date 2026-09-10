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
	// Take the ClusterRole document only; the rest of the file is templated.
	var clusterRole string
	for _, doc := range strings.Split(string(raw), "\n---") {
		if regexp.MustCompile(`(?m)^kind: ClusterRole$`).MatchString(doc) {
			clusterRole = doc
			break
		}
	}
	if clusterRole == "" {
		t.Fatal("no ClusterRole in the chart")
	}
	i := strings.Index(clusterRole, "\nrules:")
	if i < 0 {
		t.Fatal("ClusterRole has no rules")
	}
	var chart struct {
		Rules []PolicyRule `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte(clusterRole[i:]), &chart); err != nil {
		t.Fatalf("parse rules: %v", err)
	}

	key := func(r PolicyRule) string {
		return strings.Join(r.APIGroups, ",") + "|" + strings.Join(r.Resources, ",") + "|" + strings.Join(r.Verbs, ",")
	}
	inChart := map[string]bool{}
	for _, r := range chart.Rules {
		inChart[key(r)] = true
	}
	inManifest := map[string]bool{}
	for _, r := range RemoteRules {
		inManifest[key(r)] = true
	}
	for k := range inManifest {
		if !inChart[k] {
			t.Errorf("the remote manifest grants a rule the chart does not: %s", k)
		}
	}
	for k := range inChart {
		if !inManifest[k] {
			t.Errorf("the chart grants a rule the remote manifest does not, so the feature it serves fails on remote clusters: %s", k)
		}
	}
}

// The manifest is applied by hand on someone else's cluster, so it has to parse
// and it has to create the account it claims to.
func TestRemoteManifestIsValidYAML(t *testing.T) {
	docs := strings.Split(RemoteManifest(), "\n---\n")
	if len(docs) != 5 {
		t.Fatalf("got %d documents, want namespace, account, role, binding, token", len(docs))
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
	for _, want := range []string{"Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Secret"} {
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
}
