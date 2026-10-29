package reconciler

import (
	"context"
	"net"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lolozini/quetzal/internal/models"
)

func selects(t *testing.T, np *networkingv1.NetworkPolicy, podLabels map[string]string) bool {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		t.Fatalf("selector of %s: %v", np.Name, err)
	}
	return sel.Matches(labels.Set(podLabels))
}

// internetRule reports whether the policy lets its pods reach 0.0.0.0/0.
func internetRule(np *networkingv1.NetworkPolicy) bool {
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.IPBlock != nil && p.IPBlock.CIDR == "0.0.0.0/0" {
				return true
			}
		}
	}
	return false
}

// namespacePeer reports whether the policy lets its pods reach namespace ns.
func namespacePeer(np *networkingv1.NetworkPolicy, ns string) bool {
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.NamespaceSelector != nil && p.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == ns {
				return true
			}
		}
	}
	return false
}

// cidrPeer returns the port the policy lets its pods reach cidr on, and
// whether it does.
func cidrPeer(np *networkingv1.NetworkPolicy, cidr string) (int32, bool) {
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.IPBlock != nil && p.IPBlock.CIDR == cidr {
				if len(r.Ports) == 1 && r.Ports[0].Port != nil {
					return r.Ports[0].Port.IntVal, true
				}
				return 0, true
			}
		}
	}
	return 0, false
}

// The activator's policy is worth nothing unless it selects the activator's
// pods, and nothing else of the server's.
func TestActivatorPolicySelectsTheActivatorAlone(t *testing.T) {
	s, tmpl := &models.Server{Slug: "a", Namespace: "quetzal-srv-a"}, &models.Template{
		Ports: []models.PortSpec{{Port: 25565, Protocol: "TCP"}},
	}
	np := BuildActivatorNetworkPolicy(s, tmpl, false, nil)
	act := BuildActivatorDeployment(s, tmpl, ActivatorParams{Image: "quetzal:x", WakeURL: "http://p.q.svc:8080"})
	if !selects(t, np, act.Spec.Template.Labels) {
		t.Errorf("the activator policy does not select the activator's pods %v", act.Spec.Template.Labels)
	}
	game := BuildDeployment(s, tmpl, "quetzal:x", nil)
	if selects(t, np, game.Spec.Template.Labels) {
		t.Errorf("the activator policy selects the game's pods too")
	}
}

// The activator and the backup Jobs ran with no policy, so with the run of the
// cluster network: each now gets one on every pass. The activator reaches DNS
// and the panel and, being given no internet, nothing else; the Jobs take no
// connection and reach the internet, plus the backup target when it is a
// Service of the cluster.
func TestServerNamespaceFencesTheActivatorAndTheJobs(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	if err := st.SaveBackupConfig(&models.BackupConfig{Endpoint: "minio.minio.svc:9000", Bucket: "b"}, "ak", "sk", "pw"); err != nil {
		t.Fatalf("backup config: %v", err)
	}
	cl := fakeCluster(interceptor.Funcs{})
	r := newTestReconciler(cl, st)
	r.WakeURL = "http://quetzal.quetzal.svc:8080/api/internal/wake"
	if err := r.ReconcileServer(context.Background(), srv.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	act := &networkingv1.NetworkPolicy{}
	if !get(t, cl, srv.Namespace, ActivatorPolicyName, act) {
		t.Fatal("no policy for the activator")
	}
	if internetRule(act) {
		t.Error("the activator may reach the internet")
	}
	if !namespacePeer(act, "quetzal") {
		t.Errorf("the activator cannot reach the panel it wakes: %+v", act.Spec.Egress)
	}
	if len(act.Spec.Ingress) != 1 || len(act.Spec.Ingress[0].Ports) == 0 || act.Spec.Ingress[0].Ports[0].Port.IntVal != 25565 {
		t.Errorf("the activator does not take the game's port: %+v", act.Spec.Ingress)
	}

	jobs := &networkingv1.NetworkPolicy{}
	if !get(t, cl, srv.Namespace, JobsPolicyName, jobs) {
		t.Fatal("no policy for the backup Jobs")
	}
	if len(jobs.Spec.Ingress) != 0 || !hasPolicyType(jobs, networkingv1.PolicyTypeIngress) {
		t.Errorf("the Jobs take connections: %+v", jobs.Spec)
	}
	if !internetRule(jobs) {
		t.Error("the Jobs cannot reach a public S3 provider")
	}
	if !namespacePeer(jobs, "minio") {
		t.Errorf("the Jobs cannot reach the backup target's namespace: %+v", jobs.Spec.Egress)
	}
}

func hasPolicyType(np *networkingv1.NetworkPolicy, pt networkingv1.PolicyType) bool {
	for _, p := range np.Spec.PolicyTypes {
		if p == pt {
			return true
		}
	}
	return false
}

// A backup target on the LAN is outside the internet rule, and gets a way of
// its own, on its port; a public one needs none.
func TestBackupTargetPeers(t *testing.T) {
	st := reconStore(t)
	r := newTestReconciler(fakeCluster(interceptor.Funcs{}), st)
	r.Resolve = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("51.159.0.1")}, nil
	}
	for _, c := range []struct {
		endpoint string
		ssl      bool
		cidr     string
		port     int32
	}{
		{"192.168.1.10:9000", false, "192.168.1.10/32", 9000},
		{"10.1.2.3", true, "10.1.2.3/32", 443},
		{"s3.fr-par.scw.cloud", true, "", 0},
		{"192.168.1.10:4294976296", false, "", 0}, // wrapped to 9000
	} {
		if err := st.SaveBackupConfig(&models.BackupConfig{Endpoint: c.endpoint, Bucket: "b", UseSSL: c.ssl}, "ak", "sk", "pw"); err != nil {
			t.Fatalf("backup config: %v", err)
		}
		np := BuildJobsNetworkPolicy(&models.Server{Namespace: "ns"}, r.backupTargetPeers())
		if c.cidr == "" {
			if len(np.Spec.Egress) != 2 {
				t.Errorf("%s: got rules of its own: %+v", c.endpoint, np.Spec.Egress)
			}
			continue
		}
		if port, ok := cidrPeer(np, c.cidr); !ok || port != c.port {
			t.Errorf("%s: way to %s on %d = %d, %v", c.endpoint, c.cidr, c.port, port, ok)
		}
	}
}

// A panel called back at an address is reached at that address, public or
// not: the activator has no internet to reach it through.
func TestCallbackPeers(t *testing.T) {
	r := newTestReconciler(fakeCluster(interceptor.Funcs{}), reconStore(t))
	r.Resolve = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	for _, c := range []struct {
		url  string
		want EgressPeer
	}{
		{"http://quetzal.panel.svc.cluster.local:80/api/internal/wake", EgressPeer{Namespace: "panel"}},
		{"http://10.0.0.5:8080/api/internal/wake", EgressPeer{CIDR: "10.0.0.5/32", Port: 8080}},
		{"https://panel.example.org/api/internal/wake", EgressPeer{CIDR: "203.0.113.7/32", Port: 443}},
	} {
		got := r.callbackPeers(c.url)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %+v, want %+v", c.url, got, c.want)
		}
	}
	if got := r.callbackPeers("http://169.254.169.254/latest"); len(got) != 0 {
		t.Errorf("a link-local callback was given a way: %+v", got)
	}
	// A URL parses with any digits for a port; 4294975488 wrapped to 8192.
	for _, bad := range []string{"http://10.0.0.5:4294975488/", "http://10.0.0.5:70000/", "http://10.0.0.5:0/"} {
		if got := r.callbackPeers(bad); len(got) != 0 {
			t.Errorf("%s: a port no one listens on was given a way: %+v", bad, got)
		}
	}
}
