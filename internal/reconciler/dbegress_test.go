package reconciler

import (
	"context"
	"errors"
	"net"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/lolozini/quetzal/internal/models"
)

// fakeDNS answers from a table; any other name does not resolve.
func fakeDNS(table map[string][]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		addrs, ok := table[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

// An external database host at a private address was out of reach: only one
// named by a literal address got a way through, on every port. Servers now
// reach its database port, however it is named, and nothing else of it.
func TestExternalDatabaseHostIsReachedOnItsPort(t *testing.T) {
	r := &Reconciler{}
	r.Resolve = fakeDNS(map[string][]string{
		"db.lan":         {"192.168.1.50"},
		"db.example.com": {"203.0.113.5"},
		"dual.lan":       {"10.0.0.7", "fd00::7"},
		"metadata.lan":   {"169.254.169.254"},
		"mixed.example":  {"203.0.113.9", "172.20.0.4"},
	})
	for _, c := range []struct {
		name string
		host models.DatabaseHost
		want []EgressPeer
	}{
		{"a Service of the cluster", models.DatabaseHost{Host: "mariadb.infra.svc.cluster.local", Port: 3306},
			[]EgressPeer{{Namespace: "infra", Port: 3306}}},
		{"a Service, short", models.DatabaseHost{Host: "pg.data.svc", Port: 5432},
			[]EgressPeer{{Namespace: "data", Port: 5432}}},
		{"a name on the LAN", models.DatabaseHost{Host: "db.lan", Port: 5432},
			[]EgressPeer{{CIDR: "192.168.1.50/32", Port: 5432}}},
		{"a private address", models.DatabaseHost{Host: "10.1.2.3", Port: 3306},
			[]EgressPeer{{CIDR: "10.1.2.3/32", Port: 3306}}},
		{"the address servers are given", models.DatabaseHost{Host: "db.example.com", Port: 3306, ConnectHost: "db.lan", ConnectPort: 3307},
			[]EgressPeer{{CIDR: "192.168.1.50/32", Port: 3307}}},
		{"both families", models.DatabaseHost{Host: "dual.lan", Port: 3306},
			[]EgressPeer{{CIDR: "10.0.0.7/32", Port: 3306}, {CIDR: "fd00::7/128", Port: 3306}}},
		{"a public name: the internet rule covers it", models.DatabaseHost{Host: "db.example.com", Port: 3306}, nil},
		{"only what is not public", models.DatabaseHost{Host: "mixed.example", Port: 3306},
			[]EgressPeer{{CIDR: "172.20.0.4/32", Port: 3306}}},
		{"the cloud metadata endpoint", models.DatabaseHost{Host: "169.254.169.254", Port: 80}, nil},
		{"a name for it", models.DatabaseHost{Host: "metadata.lan", Port: 80}, nil},
		{"loopback", models.DatabaseHost{Host: "127.0.0.1", Port: 3306}, nil},
		{"a name that does not resolve", models.DatabaseHost{Host: "nowhere.lan", Port: 3306}, nil},
	} {
		got := r.dbHostPeers(&c.host)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
}

// And the policy says so: the peer, on that port only.
func TestDatabaseHostRuleNamesItsPort(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	pol := BuildNetworkPolicy(s, tmpl, []EgressPeer{{CIDR: "192.168.1.50/32", Port: 5432}, {Namespace: "infra", Port: 3306}})
	want := map[string]int32{"192.168.1.50/32": 5432, "infra": 3306}
	for _, rule := range pol.Spec.Egress {
		for _, to := range rule.To {
			key := ""
			switch {
			case to.IPBlock != nil:
				key = to.IPBlock.CIDR
			case to.NamespaceSelector != nil:
				key = to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			}
			port, ok := want[key]
			if !ok {
				continue
			}
			if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != port {
				t.Errorf("%s: ports %v, want only %d", key, portsOf(rule), port)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Errorf("no rule for %v", want)
	}
}

func portsOf(rule networkingv1.NetworkPolicyEgressRule) []int32 {
	var out []int32
	for _, p := range rule.Ports {
		out = append(out, p.Port.IntVal)
	}
	return out
}

// Through the server's databases, and a name looked up once for a whole resync.
func TestServerReachesItsExternalDatabase(t *testing.T) {
	st := reconStore(t)
	srv := &models.Server{Slug: "withdb-a1b2", Namespace: NamespaceFor("withdb-a1b2")}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	h := &models.DatabaseHost{Name: "lan", Kind: models.DBHostExternal, Host: "db.lan", Port: 3306, AdminUser: "root"}
	if err := st.CreateDatabaseHost(h, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateServerDatabase(&models.ServerDatabase{ServerID: srv.ID, HostID: h.ID, DatabaseName: "s1", Username: "u1", Remote: "%"}, "pw"); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	r := &Reconciler{Store: st}
	r.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
		lookups++
		return fakeDNS(map[string][]string{"db.lan": {"192.168.1.50"}})(ctx, host)
	}
	for i := 0; i < 3; i++ {
		peers := r.egressPeersFor(srv)
		if len(peers) != 1 || peers[0].CIDR != "192.168.1.50/32" || peers[0].Port != 3306 {
			t.Fatalf("peers %v, want the database host's address and port", peers)
		}
	}
	if lookups != 1 {
		t.Errorf("%d lookups for one name in a minute, want 1", lookups)
	}
}
