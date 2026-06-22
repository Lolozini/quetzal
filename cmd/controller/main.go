// Command controller reconciles the Quetzal database (source of truth) into
// native Kubernetes objects, across one or more registered clusters. It supports
// optional leader election for HA and exposes health + Prometheus metrics.
package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/remotecommand"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/backup"
	"github.com/lolozini/quetzal/internal/cluster"
	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/hibernate"
	"github.com/lolozini/quetzal/internal/metrics"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/scheduler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func main() {
	dbDriver := store.Driver(env("QUETZAL_DB_DRIVER", "sqlite"))
	dbDSN := env("QUETZAL_DB_DSN", "quetzal.db")
	resync := envDuration("QUETZAL_RESYNC", 15*time.Second)
	metricsAddr := env("QUETZAL_METRICS_ADDR", ":9090")
	leaderEnabled := env("QUETZAL_LEADER_ELECTION", "false") == "true"
	namespace := env("POD_NAMESPACE", "quetzal-system")

	st, err := store.Open(store.Config{
		Driver:    dbDriver,
		DSN:       dbDSN,
		Silent:    true,
		SecretKey: crypto.KeyFromEnv("QUETZAL_SECRET_KEY"),
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		log.Fatalf("seed templates: %v", err)
	}

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}
	// Bound every API call so an unreachable cluster can't wedge a reconcile
	// tick. Safe here because the controller never streams (console log/attach
	// live in the apiserver, which keeps its clients timeout-free).
	clusterTimeout := envDuration("QUETZAL_CLUSTER_TIMEOUT", 30*time.Second)
	cfg.Timeout = clusterTimeout
	local, err := cluster.FromConfig(cfg)
	if err != nil {
		log.Fatalf("kube clients: %v", err)
	}
	reg := cluster.New(st, local)
	reg.RequestTimeout = clusterTimeout
	if _, err := st.EnsureLocalCluster(); err != nil {
		log.Fatalf("ensure local cluster: %v", err)
	}

	// Wake-on-connect: the activator runs as the Quetzal image (QUETZAL_IMAGE)
	// and calls back to the apiserver (QUETZAL_APISERVER_URL) to wake a server.
	// Disabled when either is unset.
	apiBase := env("QUETZAL_APISERVER_URL", "")
	actCfg := activatorConfig{
		image:     env("QUETZAL_IMAGE", ""),
		wakeURL:   apiCallbackURL(apiBase, "wake"),
		activeURL: apiCallbackURL(apiBase, "active"),
		key:       crypto.KeyFromEnv("QUETZAL_SECRET_KEY"),
	}

	sched := scheduler.New(st, &executor{st: st, reg: reg})
	bmgr := backup.NewManager(st, reg)
	hmgr := hibernate.New(st, connProbe(reg))

	go serveOps(metricsAddr, st)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run := func(ctx context.Context) {
		log.Printf("quetzal-controller reconciling (db=%s, resync=%s)", dbDriver, resync)
		ticker := time.NewTicker(resync)
		defer ticker.Stop()
		tick := func() {
			reconcileAll(ctx, reg, st, actCfg)
			sched.Tick(ctx)
			bmgr.Process(ctx)
			hmgr.Tick(ctx)
			refreshClusters(ctx, reg, st)
		}
		tick()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tick()
			}
		}
	}

	if leaderEnabled {
		runWithLeaderElection(ctx, local.Clientset, namespace, run)
	} else {
		run(ctx)
	}
	log.Printf("shutting down")
}

func runWithLeaderElection(ctx context.Context, cs kubernetes.Interface, namespace string, run func(context.Context)) {
	id, _ := os.Hostname()
	if id == "" {
		id = "quetzal-controller"
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: "quetzal-controller", Namespace: namespace},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() { log.Printf("lost leadership") },
			OnNewLeader: func(leader string) {
				if leader != id {
					log.Printf("now following leader %s", leader)
				}
			},
		},
	})
}

// executor implements scheduler.Executor against the cluster registry + store.
type executor struct {
	st  *store.Store
	reg *cluster.Registry
}

func (e *executor) Start(_ context.Context, srv *models.Server) error {
	return e.st.SetDesiredState(srv.ID, models.StateRunning)
}

func (e *executor) Stop(_ context.Context, srv *models.Server) error {
	return e.st.SetDesiredState(srv.ID, models.StateStopped)
}

func (e *executor) Restart(ctx context.Context, srv *models.Server) error {
	clients, err := e.reg.For(srv.ClusterID)
	if err != nil {
		return err
	}
	return clients.Clientset.CoreV1().Pods(srv.Namespace).DeleteCollection(ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: reconciler.ServerLabel + "=" + srv.Slug})
}

func (e *executor) Command(ctx context.Context, srv *models.Server, cmd string) error {
	clients, err := e.reg.For(srv.ClusterID)
	if err != nil {
		return err
	}
	pod, err := console.FindRunningPod(ctx, clients.Clientset, srv.Namespace, srv.Slug)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return console.SendStdin(cctx, clients.Clientset, clients.Config, srv.Namespace, pod, cmd+"\n")
}

// Backup enqueues a backup operation; the backup Manager picks it up.
func (e *executor) Backup(_ context.Context, srv *models.Server) error {
	return e.st.CreateBackup(&models.Backup{
		ServerID:  srv.ID,
		Direction: models.DirBackup,
		Phase:     models.BackupPending,
	})
}

// connProbe returns a hibernation probe that counts ESTABLISHED connections on a
// server's game ports by reading /proc/net/tcp inside its running container, on
// whichever cluster the server lives.
func connProbe(reg *cluster.Registry) hibernate.ConnProbe {
	return func(ctx context.Context, srv *models.Server) (int, error) {
		clients, err := reg.For(srv.ClusterID)
		if err != nil {
			return 0, err
		}
		pod, err := console.FindRunningPod(ctx, clients.Clientset, srv.Namespace, srv.Slug)
		if err != nil {
			return 0, err
		}
		out, err := execCapture(ctx, clients.Clientset, clients.Config, srv.Namespace, pod, reconciler.WorkloadName,
			[]string{"sh", "-c", "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null"})
		if err != nil {
			return 0, err
		}
		ports := map[int32]bool{}
		for _, p := range srv.Ports {
			ports[p.Port] = true
		}
		return hibernate.CountEstablished(out, ports), nil
	}
}

// execCapture runs a command in a pod container and returns its stdout.
func execCapture(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string, cmd []string) (string, error) {
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: cmd, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// reconcileAll reconciles every server against its target cluster, then garbage
// collects orphan namespaces per cluster. Servers are grouped by cluster so each
// cluster's GC only ever sees its own servers' slugs.
// activatorConfig carries the wake-on-connect settings applied to each
// per-cluster reconciler.
type activatorConfig struct {
	image     string
	wakeURL   string
	activeURL string
	key       []byte
}

// apiCallbackURL builds an activator callback URL from the apiserver base URL
// ("" when unset, which disables wake-on-connect).
func apiCallbackURL(base, kind string) string {
	if base == "" {
		return ""
	}
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + "/api/internal/" + kind
}

func reconcileAll(ctx context.Context, reg *cluster.Registry, st *store.Store, actCfg activatorConfig) {
	servers, err := st.ListServers()
	if err != nil {
		log.Printf("list servers: %v", err)
		return
	}
	byCluster := map[uint][]models.Server{}
	// allSlugs is the GC valid-set used on EVERY cluster. Slugs are globally
	// unique and a server never changes clusters, so a managed namespace whose
	// slug has no server is always an orphan. Using the global set (rather than
	// a per-cluster set) makes GC safe even if the same physical cluster is
	// registered twice — a row with no servers won't delete the other row's
	// namespaces on the shared cluster.
	allSlugs := make(map[string]bool, len(servers))
	for i := range servers {
		s := servers[i]
		byCluster[s.ClusterID] = append(byCluster[s.ClusterID], s)
		allSlugs[s.Slug] = true
	}
	clusters, err := st.ListClusters()
	if err != nil {
		log.Printf("list clusters: %v", err)
		return
	}
	for ci := range clusters {
		c := &clusters[ci]
		clients, err := reg.For(c.ID)
		if err != nil {
			log.Printf("cluster %s unreachable, skipping reconcile: %v", c.Slug, err)
			delete(byCluster, c.ID)
			continue
		}
		rec := reconciler.New(clients.Client, st)
		rec.OnStop = onStopFor(clients)
		rec.ActivatorImage = actCfg.image
		rec.WakeURL = actCfg.wakeURL
		rec.ActiveURL = actCfg.activeURL
		rec.WakeKey = actCfg.key
		for _, s := range byCluster[c.ID] {
			if err := rec.ReconcileServer(ctx, s.ID); err != nil {
				log.Printf("reconcile server %s (cluster %s): %v", s.Slug, c.Slug, err)
			}
		}
		if err := rec.GCOrphanNamespaces(ctx, allSlugs); err != nil {
			log.Printf("gc orphan namespaces (cluster %s): %v", c.Slug, err)
		}
		delete(byCluster, c.ID)
	}
	for id, group := range byCluster {
		for _, s := range group {
			log.Printf("server %s references unknown cluster %d; skipping", s.Slug, id)
		}
	}
}

// onStopFor delivers a template's stop command to a running container (via the
// console attach path) before the workload scales down, on the given cluster.
func onStopFor(clients cluster.Clients) func(ctx context.Context, ns, slug, stopCommand string) error {
	return func(ctx context.Context, ns, slug, stopCommand string) error {
		pod, err := console.FindRunningPod(ctx, clients.Clientset, ns, slug)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return console.SendStdin(cctx, clients.Clientset, clients.Config, ns, pod, stopCommand+"\n")
	}
}

// refreshClusters probes each cluster and records its reachability/status.
func refreshClusters(ctx context.Context, reg *cluster.Registry, st *store.Store) {
	clusters, err := st.ListClusters()
	if err != nil {
		return
	}
	for ci := range clusters {
		c := &clusters[ci]
		clients, err := reg.For(c.ID)
		if err != nil {
			_ = st.SetClusterStatus(c.ID, false, "", 0, err.Error())
			continue
		}
		version, nodes, err := cluster.Probe(ctx, clients)
		if err != nil {
			_ = st.SetClusterStatus(c.ID, false, "", 0, err.Error())
			continue
		}
		_ = st.SetClusterStatus(c.ID, true, version, nodes, "")
	}
}

func serveOps(addr string, st *store.Store) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler(st))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("metrics server: %v", err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
