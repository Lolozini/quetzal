// Command apiserver serves the Quetzal HTTP API (auth, server CRUD/power, live
// console) backed by the database (source of truth) and the Kubernetes API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/metrics"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
	webui "github.com/lolozini/quetzal/web"
)

func main() {
	addr := env("QUETZAL_ADDR", ":8080")
	dbDriver := store.Driver(env("QUETZAL_DB_DRIVER", "sqlite"))
	dbDSN := env("QUETZAL_DB_DSN", "quetzal.db")

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
	if _, err := st.EnsureLocalCluster(); err != nil {
		log.Fatalf("ensure local cluster: %v", err)
	}

	// Migration-only mode (used by an init container so a single process owns
	// schema creation/seeding, avoiding a race between the apiserver and
	// controller on a fresh shared database).
	if env("QUETZAL_MIGRATE_ONLY", "") == "true" {
		log.Printf("migration complete; exiting (QUETZAL_MIGRATE_ONLY)")
		return
	}

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	apiSrv := api.New(st, cs, cfg)
	apiSrv.Secure = env("QUETZAL_SECURE_COOKIES", "") == "true"
	apiSrv.NodePortMin = envInt32("QUETZAL_NODEPORT_MIN", 0)
	apiSrv.NodePortMax = envInt32("QUETZAL_NODEPORT_MAX", 0)
	apiSrv.WakeKey = crypto.KeyFromEnv("QUETZAL_SECRET_KEY")

	// /api/* -> API; /metrics + /healthz for ops; everything else -> React SPA.
	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	root.Handle("/metrics", metrics.Handler(st))
	root.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	root.Handle("/", webui.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go gcSessions(ctx, st)

	go func() {
		log.Printf("quetzal-apiserver listening on %s (db=%s)", addr, dbDriver)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// gcSessions periodically deletes expired sessions from the database.
func gcSessions(ctx context.Context, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(); err != nil {
			log.Printf("session gc: %v", err)
		} else if n > 0 {
			log.Printf("session gc: removed %d expired sessions", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt32(key string, def int32) int32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			return int32(n)
		}
	}
	return def
}
