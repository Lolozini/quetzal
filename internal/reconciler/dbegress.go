package reconciler

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// A database on an external host at a private address -- a MariaDB on the LAN,
// or in the cluster -- was out of reach of the servers given a database on it:
// the default policy denies the private address space, and only a host named
// by a literal address got a way through, on every port of it. A host named by
// DNS got none, and nothing said so: the user was handed an address that never
// answered.
//
// Servers now reach an external host's database port, and nothing else of it,
// however it is named. A public address needs nothing: the internet rule
// covers it.

// neverReached are addresses no database is given a way to, whatever a host
// says: link-local, which holds the cloud metadata endpoint, and loopback.
var neverReached = mustCIDRs("169.254.0.0/16", "127.0.0.0/8", "fe80::/10", "::1/128")

// needsPeer are the addresses the internet rule does not cover: the private
// ranges it excludes, and IPv6, which it does not include at all.
var needsPeer = mustCIDRs(privateRanges...)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// dbHostPeer is the way to one address of a database host, if it needs one.
func dbHostPeer(ip net.IP, port int32) (EgressPeer, bool) {
	if inAny(ip, neverReached) || (ip.To4() != nil && !inAny(ip, needsPeer)) {
		return EgressPeer{}, false
	}
	bits := 128
	if ip.To4() != nil {
		ip, bits = ip.To4(), 32
	}
	return EgressPeer{CIDR: (&net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}).String(), Port: port}, true
}

// dbHostPeers lists the ways servers reach an external database host: its
// database port at each private address it has. A Service of the cluster
// named in full (<service>.<namespace>.svc...) is reached through its
// namespace, a ClusterIP being translated to the pod behind it before policy
// applies; any other name is looked up, and again on later passes, since what
// it points to may move.
func (r *Reconciler) dbHostPeers(h *models.DatabaseHost) []EgressPeer {
	host, port := strings.TrimSpace(h.ConnectHost), h.ConnectPort
	if host == "" {
		host = strings.TrimSpace(h.Host)
	}
	if port == 0 {
		port = h.Port
	}
	if host == "" || port <= 0 || port > 65535 {
		return nil
	}
	p := int32(port)
	if ip := net.ParseIP(host); ip != nil {
		if peer, ok := dbHostPeer(ip, p); ok {
			return []EgressPeer{peer}
		}
		return nil
	}
	if ns, ok := serviceNamespace(host); ok {
		return []EgressPeer{{Namespace: ns, Port: p}}
	}
	var peers []EgressPeer
	for _, ip := range r.lookup(host) {
		if peer, ok := dbHostPeer(ip, p); ok {
			peers = append(peers, peer)
		}
	}
	return peers
}

// serviceNamespace returns the namespace of a Service named in full:
// <service>.<namespace>.svc, possibly followed by the cluster's domain.
func serviceNamespace(host string) (string, bool) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	if len(labels) >= 3 && labels[2] == "svc" && labels[0] != "" && labels[1] != "" {
		return labels[1], true
	}
	return "", false
}

// hostLookupTTL is how long a database host's addresses are reused, so a
// resync of every server does not ask DNS for each of them.
const hostLookupTTL = time.Minute

type hostAddrs struct {
	ips     []net.IP
	expires time.Time
}

// lookup resolves a database host's name, through a short cache. A name that
// does not resolve gives no way through, and is said in the log.
func (r *Reconciler) lookup(host string) []net.IP {
	r.lookupMu.Lock()
	if a, ok := r.lookups[host]; ok && time.Now().Before(a.expires) {
		r.lookupMu.Unlock()
		return a.ips
	}
	r.lookupMu.Unlock()

	resolve := r.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := resolve(ctx, host)
	if err != nil {
		log.Printf("database host %q does not resolve, so no server can reach it: %v", host, err)
		ips = nil
	}
	r.lookupMu.Lock()
	if r.lookups == nil {
		r.lookups = map[string]hostAddrs{}
	}
	r.lookups[host] = hostAddrs{ips: ips, expires: time.Now().Add(hostLookupTTL)}
	r.lookupMu.Unlock()
	return ips
}

// lookupState holds the lookup cache; embedded in Reconciler.
type lookupState struct {
	lookupMu sync.Mutex
	lookups  map[string]hostAddrs
	// Resolve looks a host name up; net.DefaultResolver when nil. Replaceable
	// in tests.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}
