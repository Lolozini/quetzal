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
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

func main() {
	addr := env("QUETZAL_ADDR", ":8080")
	dbDriver := store.Driver(env("QUETZAL_DB_DRIVER", "sqlite"))
	dbDSN := env("QUETZAL_DB_DSN", "quetzal.db")

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
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	apiSrv := api.New(st, cs, cfg)
	apiSrv.Secure = env("QUETZAL_SECURE_COOKIES", "") == "true"

	srv := &http.Server{
		Addr:              addr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
