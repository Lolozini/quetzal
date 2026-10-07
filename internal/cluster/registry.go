// Package cluster resolves per-cluster Kubernetes clients from the database,
// so the control plane can deploy and manage servers across multiple clusters.
// The local cluster reuses the control plane's own (in-cluster / default)
// config; remote clusters are built from a stored kubeconfig and cached until
// the kubeconfig changes.
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/store"
)

// Clients bundles the Kubernetes clients for one cluster.
type Clients struct {
	Client    client.Client        // controller-runtime (server-side apply)
	Clientset kubernetes.Interface // client-go (exec/attach/logs/jobs)
	Config    *rest.Config
}

type cached struct {
	clients Clients
	hash    string // kubeconfig hash, so a credential change invalidates the entry
}

// Registry hands out cached k8s clients keyed by cluster ID.
type Registry struct {
	store *store.Store
	local Clients
	// RequestTimeout, when >0, bounds every API call on remote clients built by
	// this registry. The controller sets it (it never streams) so an unreachable
	// remote cannot wedge the reconcile loop; the apiserver leaves it 0 because
	// it serves long-lived console log/attach streams.
	RequestTimeout time.Duration
	mu             sync.Mutex
	cache          map[uint]cached
}

// New returns a registry whose local cluster uses the given clients.
func New(st *store.Store, local Clients) *Registry {
	return &Registry{store: st, local: local, cache: map[uint]cached{}}
}

// Local returns the control plane's own cluster clients.
func (r *Registry) Local() Clients { return r.local }

// For returns the clients for a cluster ID. ID 0 or an in-cluster cluster maps
// to the local clients; remote clusters are built from their stored kubeconfig.
func (r *Registry) For(id uint) (Clients, error) {
	if id == 0 {
		return r.local, nil
	}
	c, err := r.store.GetCluster(id)
	if err != nil {
		return Clients{}, err
	}
	if c.InCluster {
		return r.local, nil
	}
	kubeconfig, err := r.store.ClusterKubeconfig(c)
	if err != nil {
		return Clients{}, err
	}
	if kubeconfig == "" {
		return Clients{}, fmt.Errorf("cluster %q has no kubeconfig", c.Slug)
	}
	h := hashStr(kubeconfig)

	r.mu.Lock()
	defer r.mu.Unlock()
	if ent, ok := r.cache[id]; ok && ent.hash == h {
		return ent.clients, nil
	}
	clients, err := buildWithTimeout(kubeconfig, r.RequestTimeout)
	if err != nil {
		return Clients{}, err
	}
	r.cache[id] = cached{clients: clients, hash: h}
	return clients, nil
}

// Build constructs clients from a raw kubeconfig (no caching, no request
// timeout). Used to validate a kubeconfig before storing it.
func Build(kubeconfig string) (Clients, error) {
	return buildWithTimeout(kubeconfig, 0)
}

func buildWithTimeout(kubeconfig string, timeout time.Duration) (Clients, error) {
	// Parse without resolving credentials: remote kubeconfigs are supplied by
	// cluster administrators, not trusted configuration of the panel process.
	raw, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return Clients{}, err
	}
	for name, auth := range raw.AuthInfos {
		switch {
		case auth.TokenFile != "":
			return Clients{}, fmt.Errorf("kubeconfig user %q: tokenFile is not permitted", name)
		case auth.ClientCertificate != "" || auth.ClientKey != "":
			return Clients{}, fmt.Errorf("kubeconfig user %q: local client certificate/key files are not permitted; embed their data instead", name)
		case auth.Exec != nil:
			return Clients{}, fmt.Errorf("kubeconfig user %q: exec credentials are not permitted", name)
		case auth.AuthProvider != nil:
			return Clients{}, fmt.Errorf("kubeconfig user %q: auth-provider credentials are not permitted", name)
		}
	}
	for name, cluster := range raw.Clusters {
		if cluster.CertificateAuthority != "" {
			return Clients{}, fmt.Errorf("kubeconfig cluster %q: local certificate-authority files are not permitted; embed their data instead", name)
		}
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return Clients{}, err
	}
	if timeout > 0 {
		cfg.Timeout = timeout
	}
	return FromConfig(cfg)
}

// FromConfig builds clients from an existing rest.Config (the local cluster).
func FromConfig(cfg *rest.Config) (Clients, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, err
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return Clients{}, err
	}
	return Clients{Client: cl, Clientset: cs, Config: cfg}, nil
}

// Probe checks reachability and returns the server version and node count. It is
// bounded by ctx (so an unreachable endpoint fails fast); we deliberately do not
// put a timeout on the shared rest.Config, which is reused for long-lived console
// log/attach streams.
func Probe(ctx context.Context, c Clients) (version string, nodes int, err error) {
	raw, err := c.Clientset.Discovery().RESTClient().Get().AbsPath("/version").DoRaw(ctx)
	if err != nil {
		return "", 0, loopbackHint(c.Config, err)
	}
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	_ = json.Unmarshal(raw, &v)
	nl, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return v.GitVersion, 0, nil // reachable, but can't list nodes (RBAC)
	}
	return v.GitVersion, len(nl.Items), nil
}

// loopbackHint adds the likely cause to a failure to reach a cluster registered
// at a loopback address. Such an address comes from a kubeconfig that works on
// the operator's machine -- through a tunnel, or to a kind cluster -- and from
// Quetzal it leads back to Quetzal's own pod.
func loopbackHint(cfg *rest.Config, err error) error {
	if cfg == nil {
		return err
	}
	host := cfg.Host
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, perr := url.Parse(host)
	if perr != nil {
		return err
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return err
	}
	return fmt.Errorf("%w (%s is a loopback address, which from Quetzal leads back to Quetzal itself: put an address of the cluster that Quetzal can reach in the kubeconfig)", err, h)
}

func hashStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
