package reconciler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// A proxy's NetworkPolicy lets it reach the namespaces of the servers it was
// given (their own policy admits only their game ports), and no others: not a
// server deleted since, nor one moved to another cluster.
func TestProxyReachesTheServersItWasGiven(t *testing.T) {
	st := reconStore(t)
	mk := func(slug string, cluster uint) *models.Server {
		s := &models.Server{Slug: slug, Namespace: NamespaceFor(slug), ClusterID: cluster}
		if err := st.CreateServer(s); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		return s
	}
	lobby, survival, far := mk("lobby-a1b2", 0), mk("survival-c3d4", 0), mk("far-e5f6", 7)
	other := mk("unrelated-g7h8", 0)
	proxy := &models.Server{Slug: "proxy-i9j0", Namespace: NamespaceFor("proxy-i9j0"),
		Reaches: []string{lobby.Slug, survival.Slug, far.Slug, "deleted-k1l2"}}
	r := &Reconciler{Store: st}

	peers := map[string]bool{}
	for _, p := range r.egressPeersFor(proxy) {
		peers[p.Namespace] = true
	}
	for _, s := range []*models.Server{lobby, survival} {
		if !peers[s.Namespace] {
			t.Errorf("the proxy cannot reach %s", s.Slug)
		}
	}
	for _, s := range []*models.Server{far, other} {
		if peers[s.Namespace] {
			t.Errorf("the proxy reaches %s", s.Slug)
		}
	}
	if len(peers) != 2 {
		t.Errorf("egress peers %v, want the two servers it was given", peers)
	}

	s, tmpl := testServerAndTemplate()
	pol := BuildNetworkPolicy(s, tmpl, []EgressPeer{{Namespace: lobby.Namespace}})
	found := false
	for _, rule := range pol.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector != nil && to.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] == lobby.Namespace {
				found = true
			}
		}
	}
	if !found {
		t.Error("no egress rule for the reached server's namespace")
	}
}
