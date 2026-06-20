// Command controller reconciles the Quetzal database (source of truth) into
// native Kubernetes objects. It supports optional leader election for HA and
// exposes health + Prometheus metrics endpoints.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/console"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/metrics"
	"github.com/lolozini/quetzal/internal/reconciler"
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
	crClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("clientset: %v", err)
	}

	rec := reconciler.New(crClient, st)
	// Graceful stop: deliver the template stop command to the container's stdin
	// (via the console attach path) before the workload is scaled down.
	rec.OnStop = func(ctx context.Context, ns, slug, stopCommand string) error {
		pod, err := console.FindRunningPod(ctx, cs, ns, slug)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return console.SendStdin(cctx, cs, cfg, ns, pod, stopCommand+"\n")
	}

	go serveOps(metricsAddr, st)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run := func(ctx context.Context) {
		log.Printf("quetzal-controller reconciling (db=%s, resync=%s)", dbDriver, resync)
		ticker := time.NewTicker(resync)
		defer ticker.Stop()
		reconcileAll(ctx, rec, st)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileAll(ctx, rec, st)
			}
		}
	}

	if leaderEnabled {
		runWithLeaderElection(ctx, cs, namespace, run)
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

func reconcileAll(ctx context.Context, rec *reconciler.Reconciler, st *store.Store) {
	servers, err := st.ListServers()
	if err != nil {
		log.Printf("list servers: %v", err)
		return
	}
	valid := make(map[string]bool, len(servers))
	for i := range servers {
		s := servers[i]
		valid[s.Slug] = true
		if err := rec.ReconcileServer(ctx, s.ID); err != nil {
			log.Printf("reconcile server %s: %v", s.Slug, err)
		}
	}
	if err := rec.GCOrphanNamespaces(ctx, valid); err != nil {
		log.Printf("gc orphan namespaces: %v", err)
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
