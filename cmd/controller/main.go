// Command controller reconciles the Quetzal database (source of truth) into
// native Kubernetes objects. Phase 0: a simple resync loop over all servers.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func main() {
	dbDriver := store.Driver(env("QUETZAL_DB_DRIVER", "sqlite"))
	dbDSN := env("QUETZAL_DB_DSN", "quetzal.db")
	resync := envDuration("QUETZAL_RESYNC", 15*time.Second)

	st, err := store.Open(store.Config{Driver: dbDriver, DSN: dbDSN, Silent: true})
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
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	rec := reconciler.New(c, st)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("quetzal-controller started (db=%s, resync=%s)", dbDriver, resync)
	ticker := time.NewTicker(resync)
	defer ticker.Stop()

	reconcileAll(ctx, rec, st)
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return
		case <-ticker.C:
			reconcileAll(ctx, rec, st)
		}
	}
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
			continue
		}
		log.Printf("reconciled server %s (state=%s)", s.Slug, s.DesiredState)
	}
	if err := rec.GCOrphanNamespaces(ctx, valid); err != nil {
		log.Printf("gc orphan namespaces: %v", err)
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
