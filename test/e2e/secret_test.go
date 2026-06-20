//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/lolozini/quetzal/internal/api"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

// TestE2ESecretEnv verifies that a secret template variable is encrypted in the
// DB, materialized into a Kubernetes Secret, and referenced via secretKeyRef —
// never stored or rendered in clear text.
func TestE2ESecretEnv(t *testing.T) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	crClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("ctrl client: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	st, err := store.Open(store.Config{
		Driver:    store.DriverSQLite,
		DSN:       filepath.Join(t.TempDir(), "secret.db"),
		Silent:    true,
		SecretKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := reconciler.New(crClient, st)

	ts := httptest.NewServer(api.New(st, cs, cfg).Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	mustStatus(t, doPost(t, hc, ts.URL+"/api/setup", map[string]string{"username": "admin", "password": "supersecret"}), http.StatusCreated)

	const secretVal = "topsecretrcon"
	// minecraft-paper declares RCON_PASSWORD as a secret variable; use the tiny
	// pause image so we don't pull a large game image.
	var created struct{ ID uint }
	resp := doPost(t, hc, ts.URL+"/api/servers", map[string]any{
		"name":     "secret e2e",
		"template": "minecraft-paper",
		"image":    pauseImage,
		"start":    true,
		"env":      map[string]string{"RCON_PASSWORD": secretVal},
	})
	mustStatus(t, resp, http.StatusCreated)
	json.NewDecoder(resp.Body).Decode(&created)
	t.Cleanup(func() {
		if srv, err := st.GetServer(created.ID); err == nil {
			_ = rec.DeleteServer(context.Background(), srv)
		}
	})

	reconcileUntilRunning(context.Background(), t, rec, st, created.ID)
	ctx := context.Background()

	srv, err := st.GetServer(created.ID)
	if err != nil {
		t.Fatalf("get server: %v", err)
	}

	// 1) DB: secret not in clear env; encrypted blob present.
	if _, ok := srv.Env["RCON_PASSWORD"]; ok {
		t.Error("RCON_PASSWORD must not be in the clear-text Env map")
	}
	if !strings.HasPrefix(srv.SecretEnvEnc, "enc:") {
		t.Errorf("SecretEnvEnc should be encrypted (enc:), got prefix of %q", srv.SecretEnvEnc)
	}
	if strings.Contains(srv.SecretEnvEnc, secretVal) {
		t.Error("encrypted blob leaks the secret value")
	}

	// 2) Kubernetes Secret materialized with the value.
	sec := &corev1.Secret{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: "server-env"}, sec); err != nil {
		t.Fatalf("get k8s secret: %v", err)
	}
	if string(sec.Data["RCON_PASSWORD"]) != secretVal {
		t.Errorf("secret value = %q, want %q", sec.Data["RCON_PASSWORD"], secretVal)
	}

	// 3) Deployment references it via secretKeyRef (not a plain value).
	dep := &appsv1.Deployment{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: srv.Namespace, Name: "server"}, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	found := false
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "RCON_PASSWORD" {
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("RCON_PASSWORD should use secretKeyRef, got %+v", e)
			}
			found = true
		}
	}
	if !found {
		t.Error("RCON_PASSWORD env not found on the deployment")
	}
}
