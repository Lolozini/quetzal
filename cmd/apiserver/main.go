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
	// Embed the IANA zone database: the runtime image is distroless and has
	// no /usr/share/zoneinfo, so a schedule's named time zone would not load.
	_ "time/tzdata"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/metrics"
	"github.com/lolozini/quetzal/internal/notify"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/version"
	webui "github.com/lolozini/quetzal/web"
)

func main() {
	version.HandleFlag()
	log.Printf("starting %s", version.String())
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
	if _, err := st.EnsureLocalCluster(); err != nil {
		log.Fatalf("ensure local cluster: %v", err)
	}

	// Migration-only mode (used by an init container so a single process owns
	// schema creation, avoiding a race between the apiserver and controller on a
	// fresh shared database).
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
	// Brute-force counters go to the database: they then survive a restart (an
	// upgrade would otherwise hand out a fresh budget) and are shared, so the
	// configured limit stays the limit however many replicas run.
	apiSrv.LoginLimiter.Share(st, "login:")
	apiSrv.AuthIPLimiter.Share(st, "ip:")
	apiSrv.ForgotLimiter.Share(st, "forgot:")
	apiSrv.Secure = env("QUETZAL_SECURE_COOKIES", "") == "true"
	apiSrv.NodePortMin = envInt32("QUETZAL_NODEPORT_MIN", 0)
	apiSrv.NodePortMax = envInt32("QUETZAL_NODEPORT_MAX", 0)
	apiSrv.WakeKey = crypto.KeyFromEnv("QUETZAL_SECRET_KEY")
	apiSrv.TrustProxy = env("QUETZAL_TRUST_PROXY", "") == "true"
	// Only for `npm run dev` against this API; a deployed panel leaves it unset.
	apiSrv.DevOrigin = env("QUETZAL_DEV_ORIGIN", "") == "true"
	// Where to run Jobs that must outlive the namespace they act on.
	apiSrv.Namespace = env("POD_NAMESPACE", "")

	// The notification dispatcher drains the event outbox to configured channels.
	dispatcher := notify.New(st)
	apiSrv.Dispatch = dispatcher

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
		Addr: addr,
		// Wrap everything, not just /api: the SPA and the file bytes it links to
		// share this origin with the session cookie.
		Handler:           apiSrv.SecurityHeaders(root),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go gcSessions(ctx, st, logRetention{
		events: envInt("QUETZAL_EVENT_RETENTION_DAYS", 30),
		audit:  envInt("QUETZAL_AUDIT_RETENTION_DAYS", 0),
	})
	go gcRateLimiters(ctx, apiSrv)
	go dispatcher.Run(ctx)

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

// gcSessions periodically deletes expired sessions and password-reset tokens,
// and prunes the log tables per their retention settings.
func gcSessions(ctx context.Context, st *store.Store, retention logRetention) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(); err != nil {
			log.Printf("session gc: %v", err)
		} else if n > 0 {
			log.Printf("session gc: removed %d expired sessions", n)
		}
		if _, err := st.DeleteExpiredPasswordResets(); err != nil {
			log.Printf("reset-token gc: %v", err)
		}
		gcLogs(st, retention)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// logRetention is how long the two append-only tables are kept, in days. 0
// disables pruning for that table.
type logRetention struct {
	events int
	audit  int
}

// gcLogs prunes the event outbox and, when asked, the audit log. Events are
// pruned by default: the table is written on every power action, crash and
// restart, read by the dispatcher through a cursor, and was never emptied. The
// audit log defaults to keeping everything, because deleting an accountability
// record is the operator's call, not a default.
func gcLogs(st *store.Store, r logRetention) {
	if r.events > 0 {
		// Never past the dispatcher's cursor: an event that has not gone out yet
		// would become a notification nobody receives.
		cursor, err := st.NotifyCursor()
		if err != nil {
			log.Printf("event gc: read delivery cursor: %v", err)
		} else if n, err := st.DeleteEventsBefore(time.Now().AddDate(0, 0, -r.events), cursor); err != nil {
			log.Printf("event gc: %v", err)
		} else if n > 0 {
			log.Printf("event gc: removed %d delivered events older than %d days", n, r.events)
		}
	}
	if r.audit > 0 {
		if n, err := st.DeleteAuditBefore(time.Now().AddDate(0, 0, -r.audit)); err != nil {
			log.Printf("audit gc: %v", err)
		} else if n > 0 {
			log.Printf("audit gc: removed %d audit entries older than %d days", n, r.audit)
		}
	}
}

// gcRateLimiters periodically drops expired rate-limit counters.
func gcRateLimiters(ctx context.Context, srv *api.Server) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			srv.GCRateLimiters()
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
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
