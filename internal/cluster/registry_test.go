package cluster

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/internal/testdb"
)

func boundaryKubeconfig(t *testing.T, server string, user, cluster map[string]any) string {
	t.Helper()
	if cluster == nil {
		cluster = map[string]any{}
	}
	cluster["server"] = server
	data, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Config", "current-context": "test",
		"clusters": []any{map[string]any{"name": "test", "cluster": cluster}},
		"users":    []any{map[string]any{"name": "test", "user": user}},
		"contexts": []any{map[string]any{"name": "test", "context": map[string]any{"cluster": "test", "user": "test"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBuildRejectsTokenFileBeforeProbe(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "harmless-token")
	if err := os.WriteFile(secret, []byte("audit-local-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	var leaked atomic.Bool
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "Bearer audit-local-secret" {
			leaked.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"gitVersion":"audit","kind":"NodeList","apiVersion":"v1","items":[]}`)
	}))
	defer endpoint.Close()
	clients, err := Build(boundaryKubeconfig(t, endpoint.URL, map[string]any{"tokenFile": secret}, map[string]any{"insecure-skip-tls-verify": true}))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = Probe(ctx, clients)
		t.Error("accepted tokenFile from an untrusted kubeconfig")
	}
	if leaked.Load() {
		t.Error("local token was sent to the kubeconfig endpoint")
	}
}

func TestBuildRejectsLocalCredentialsBeforeSideEffects(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "must-not-read")
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	for _, tc := range []struct {
		name    string
		user    map[string]any
		cluster map[string]any
	}{
		{"tokenFile", map[string]any{"tokenFile": missing}, nil},
		{"client-certificate", map[string]any{"client-certificate": missing, "client-key": missing}, nil},
		{"client-key", map[string]any{"client-key": missing}, nil},
		{"certificate-authority", nil, map[string]any{"certificate-authority": missing}},
		{"exec", map[string]any{"exec": map[string]any{"apiVersion": "client.authentication.k8s.io/v1", "command": "/bin/sh", "args": []string{"-c", "touch " + marker}, "interactiveMode": "Never"}}, nil},
		{"auth-provider", map[string]any{"auth-provider": map[string]any{"name": "oidc"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(boundaryKubeconfig(t, "https://127.0.0.1:1", tc.user, tc.cluster))
			if err == nil || !strings.Contains(err.Error(), "not permitted") {
				t.Errorf("want policy rejection before credential loading, got %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("credential command executed: %v", err)
			}
		})
	}
}

func TestBuildAllowsInlineTokenAndTrustedLocalConfig(t *testing.T) {
	clients, err := Build(boundaryKubeconfig(t, "https://127.0.0.1:1", map[string]any{"token": "inline-token"}, nil))
	if err != nil || clients.Config.BearerToken != "inline-token" {
		t.Fatalf("inline credentials rejected: %v", err)
	}
	cfg := &rest.Config{Host: "https://127.0.0.1:1", BearerToken: "trusted", BearerTokenFile: filepath.Join(t.TempDir(), "trusted-token")}
	if err := os.WriteFile(cfg.BearerTokenFile, []byte("trusted"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := FromConfig(cfg)
	if err != nil || local.Config != cfg {
		t.Fatalf("trusted local config was restricted: %v", err)
	}
}

func TestRegistryRejectsStoredLocalCredentials(t *testing.T) {
	st, err := store.Open(store.Config{
		Driver: store.Driver(testdb.Driver()), DSN: testdb.DSN(t, "registry.db"),
		Silent: true, SecretKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	c := &models.Cluster{Name: "remote", Slug: "remote"}
	safe := boundaryKubeconfig(t, "https://127.0.0.1:1", map[string]any{"token": "inline"}, nil)
	if err := st.CreateCluster(c, safe); err != nil {
		t.Fatal(err)
	}
	reg := New(st, Clients{})
	if _, err := reg.For(c.ID); err != nil {
		t.Fatal(err)
	}
	unsafe := boundaryKubeconfig(t, "https://127.0.0.1:1", map[string]any{
		"exec": map[string]any{"apiVersion": "client.authentication.k8s.io/v1", "command": "/bin/false", "interactiveMode": "Never"},
	}, nil)
	if err := st.UpdateCluster(c.ID, c.Name, unsafe, nil, nil); err != nil {
		t.Fatal(err)
	}
	for name, registry := range map[string]*Registry{"cached": reg, "fresh": New(st, Clients{})} {
		if _, err := registry.For(c.ID); err == nil || !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("%s registry accepted stored external credentials: %v", name, err)
		}
	}
}

func TestBuildAllowsInlineCertificates(t *testing.T) {
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, `{"gitVersion":"inline","kind":"NodeList","apiVersion":"v1","items":[]}`)
	}))
	defer endpoint.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw})
	key, err := x509.MarshalPKCS8PrivateKey(endpoint.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := Build(boundaryKubeconfig(t, endpoint.URL, map[string]any{
		"client-certificate-data": base64.StdEncoding.EncodeToString(ca),
		"client-key-data":         base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
	}, map[string]any{
		"certificate-authority-data": base64.StdEncoding.EncodeToString(ca),
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := Probe(ctx, clients); err != nil {
		t.Fatalf("inline certificates failed: %v", err)
	}
}
