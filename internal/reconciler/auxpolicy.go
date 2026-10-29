package reconciler

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
)

// The activator and the backup Jobs run in a server's namespace next to the
// game, but with no policy of their own, so with ingress and egress open: the
// Jobs read and write the players' files, and a compromised activator, which
// takes the game's connections, could have reached anything on the cluster
// network. Each now has a policy letting through what it does and nothing else.
const (
	ActivatorPolicyName = "quetzal-activator"
	JobsPolicyName      = "quetzal-jobs"
)

// BuildActivatorNetworkPolicy fences a server's activator: in, the game's ports,
// from anywhere, as the game's own policy has them; out, DNS, the panel it calls
// back (panel), and in proxy mode the game it forwards to, on those ports. It
// is given no internet: it runs no code but Quetzal's, and needs none.
func BuildActivatorNetworkPolicy(s *models.Server, t *models.Template, proxy bool, panel []EgressPeer) *networkingv1.NetworkPolicy {
	var ports []networkingv1.NetworkPolicyPort
	for _, p := range serverPorts(s, t) {
		proto := protocol(p.Protocol)
		port := intstr.FromInt32(p.Port)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	udp, tcp, dns := corev1.ProtocolUDP, corev1.ProtocolTCP, intstr.FromInt32(53)
	egress := []networkingv1.NetworkPolicyEgressRule{dnsRule(udp, tcp, dns, panel)}
	egress = append(egress, peerRules(panel)...)
	if proxy && len(ports) > 0 {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{netpolLabel: netpolRestricted}},
			}},
			Ports: ports,
		})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: ActivatorPolicyName, Namespace: s.Namespace, Labels: labelsFor(s)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: ActivatorLabel, Operator: metav1.LabelSelectorOpExists,
			}}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingressRules(ports),
			Egress:      egress,
		},
	}
}

// BuildJobsNetworkPolicy fences a server's backup, restore, import and snapshot
// deletion Jobs: nothing in; out, DNS, the public internet an S3 provider is
// on, and extra: the backup target when it is private, the server's databases
// for a dump, what the operator allows every server.
func BuildJobsNetworkPolicy(s *models.Server, extra []EgressPeer) *networkingv1.NetworkPolicy {
	udp, tcp, dns := corev1.ProtocolUDP, corev1.ProtocolTCP, intstr.FromInt32(53)
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: JobsPolicyName, Namespace: s.Namespace, Labels: labelsFor(s)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: BackupJobLabel, Operator: metav1.LabelSelectorOpExists,
			}}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egressRules(udp, tcp, dns, extra),
		},
	}
}

// ensureAuxPolicies applies the activator's and the Jobs' policies. Both exist
// whether or not an activator or a Job does, so that neither ever starts
// before its policy.
func (r *Reconciler) ensureAuxPolicies(ctx context.Context, s *models.Server, t *models.Template) error {
	if err := r.apply(ctx, BuildActivatorNetworkPolicy(s, t, r.proxyActive(s, t), r.callbackPeers(ctx, r.WakeURL))); err != nil {
		return err
	}
	return r.apply(ctx, BuildJobsNetworkPolicy(s, append(r.peersFor(s, false), r.backupTargetPeers()...)))
}

// callbackPeers are the ways to the panel at rawURL: when the URL names its
// Service, the pods behind it on the port the Service sends that one to, else
// each address the name has, public ones too, the activator having no internet.
func (r *Reconciler) callbackPeers(ctx context.Context, rawURL string) []EgressPeer {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := u.Hostname()
	port, ok := portOr(u.Port(), defaultPort(u.Scheme == "https"))
	if !ok {
		return nil
	}
	if ns, ok := serviceNamespace(host); ok {
		return []EgressPeer{r.servicePeer(ctx, ns, strings.SplitN(host, ".", 2)[0], port)}
	}
	var peers []EgressPeer
	for _, ip := range r.addresses(host) {
		if !inAny(ip, neverReached) {
			peers = append(peers, EgressPeer{CIDR: hostCIDR(ip), Port: port})
		}
	}
	return peers
}

// servicePeer is the way to the pods behind Service ns/name, on the port its
// port carries traffic to. The whole namespace, on any port, was the way: the
// port a Service listens on need not be its pods', and the activator -- in a
// tenant's namespace, next to the game -- could reach every pod of the panel's,
// its database included when it lives there. A Service that cannot be read, or
// that selects nothing, keeps that.
func (r *Reconciler) servicePeer(ctx context.Context, ns, name string, port int32) EgressPeer {
	key := ns + "/" + name + ":" + strconv.Itoa(int(port))
	r.servicePeersMu.Lock()
	defer r.servicePeersMu.Unlock()
	if p, ok := r.servicePeers[key]; ok {
		return p
	}
	peer := EgressPeer{Namespace: ns}
	var svc corev1.Service
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
		r.warnOnce("service-peer:"+key, "wake URL names Service %s/%s, which I cannot read (%v): the activators may reach every pod of %s", ns, name, err, ns)
	} else if len(svc.Spec.Selector) > 0 {
		for _, sp := range svc.Spec.Ports {
			if sp.Port != port || (sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP) {
				continue
			}
			peer.Pods = svc.Spec.Selector
			switch {
			case sp.TargetPort.Type == intstr.String && sp.TargetPort.StrVal != "":
				peer.PortName = sp.TargetPort.StrVal
			case sp.TargetPort.IntVal != 0:
				peer.Port = sp.TargetPort.IntVal
			default:
				peer.Port = sp.Port
			}
			break
		}
	}
	if r.servicePeers == nil {
		r.servicePeers = map[string]EgressPeer{}
	}
	r.servicePeers[key] = peer
	return peer
}

// defaultPort is HTTP's or HTTPS's.
func defaultPort(tls bool) int32 {
	if tls {
		return 443
	}
	return 80
}

// portOr reads the port of an address an admin typed, or gives def when there
// is none. One out of range, which a URL parses, gets no way at all: whatever
// is meant to answer there cannot.
func portOr(s string, def int32) (int32, bool) {
	if s == "" {
		return def, true
	}
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil || p == 0 {
		return 0, false
	}
	return int32(p), true
}

// backupTargetPeers are the ways to the S3 endpoint backups go to when it is on
// a private address, which the internet rule does not cover: a MinIO in the
// cluster, a NAS on the LAN. A Service of the cluster is reached through its
// namespace, on any port: the one it listens on may not be the Service's.
func (r *Reconciler) backupTargetPeers() []EgressPeer {
	if r.Store == nil {
		return nil
	}
	cfg, err := r.Store.GetBackupConfig()
	if err != nil || cfg == nil || strings.TrimSpace(cfg.Endpoint) == "" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(cfg.Endpoint))
	if err != nil {
		host, portStr = strings.TrimSpace(cfg.Endpoint), ""
	}
	port, ok := portOr(portStr, defaultPort(cfg.UseSSL))
	if !ok {
		return nil
	}
	if ns, ok := serviceNamespace(host); ok {
		return []EgressPeer{{Namespace: ns}}
	}
	var peers []EgressPeer
	for _, ip := range r.addresses(host) {
		if peer, ok := dbHostPeer(ip, port); ok {
			peers = append(peers, peer)
		}
	}
	return peers
}

// addresses is host itself when it is an address, else what it resolves to.
func (r *Reconciler) addresses(host string) []net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	return r.lookup(host)
}

// hostCIDR is the block holding ip alone.
func hostCIDR(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return (&net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}).String()
	}
	return (&net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}).String()
}
