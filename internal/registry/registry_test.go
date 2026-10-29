package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParse(t *testing.T) {
	for ref, want := range map[string][3]string{
		"alpine:3.24":                      {"registry-1.docker.io", "library/alpine", "3.24"},
		"itzg/minecraft-server":            {"registry-1.docker.io", "itzg/minecraft-server", "latest"},
		"docker.io/library/mariadb:12.3":   {"registry-1.docker.io", "library/mariadb", "12.3"},
		"ghcr.io/ptero-eggs/yolks:java_21": {"ghcr.io", "ptero-eggs/yolks", "java_21"},
		"localhost:5000/games/ts":          {"localhost:5000", "games/ts", "latest"},
		"registry.example.org:5000/a/b:v1": {"registry.example.org:5000", "a/b", "v1"},
	} {
		host, repo, tag, err := parse(ref)
		if err != nil || [3]string{host, repo, tag} != want {
			t.Errorf("%s: %s %s %s %v, want %v", ref, host, repo, tag, err, want)
		}
	}
	for _, ref := range []string{"", "alpine@" + digest, "ghcr.io/a/b:v1@" + digest, "bad ref"} {
		if _, _, _, err := parse(ref); err == nil {
			t.Errorf("%q was taken for a tag", ref)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull,push"`)
	if scheme != "Bearer" || p["realm"] != "https://auth.docker.io/token" ||
		p["service"] != "registry.docker.io" || p["scope"] != "repository:library/alpine:pull,push" {
		t.Errorf("%s %v", scheme, p)
	}
}

// registry is a registry that wants an anonymous token, as Docker Hub and GHCR
// do for a public image.
func registry(t *testing.T, manifest func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:eggs/yolks:pull" || r.URL.Query().Get("service") != "test" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"token":"anon"}`))
	})
	mux.HandleFunc("/v2/eggs/yolks/manifests/java_21", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:eggs/yolks:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodHead || !strings.Contains(r.Header.Get("Accept"), "image.index") {
			http.Error(w, "not what a kubelet asks", http.StatusBadRequest)
			return
		}
		manifest(w, r)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDigestThroughAnAnonymousToken(t *testing.T) {
	srv := registry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", digest)
	})
	r := &Resolver{Client: srv.Client()}
	ref := strings.TrimPrefix(srv.URL, "https://") + "/eggs/yolks:java_21"
	got, err := r.Digest(context.Background(), ref)
	if err != nil || got != digest {
		t.Fatalf("Digest = %q, %v", got, err)
	}
}

func TestDigestRefusals(t *testing.T) {
	for name, manifest := range map[string]func(http.ResponseWriter, *http.Request){
		"no digest":   func(http.ResponseWriter, *http.Request) {},
		"not sha256":  func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Docker-Content-Digest", "md5:abc") },
		"unknown tag": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
	} {
		srv := registry(t, manifest)
		r := &Resolver{Client: srv.Client()}
		if d, err := r.Digest(context.Background(), strings.TrimPrefix(srv.URL, "https://")+"/eggs/yolks:java_21"); err == nil {
			t.Errorf("%s: got %q", name, d)
		}
	}
}

// The registry names its token server; one inside the network is not called.
func TestDigestStaysOffPrivateAddresses(t *testing.T) {
	srv := registry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", digest)
	})
	var r Resolver // the default client: public addresses only
	if d, err := r.Digest(context.Background(), strings.TrimPrefix(srv.URL, "https://")+"/eggs/yolks:java_21"); err == nil {
		t.Errorf("a registry on loopback answered: %q", d)
	}
}

func TestTokenRealmMustBeHTTPS(t *testing.T) {
	var r Resolver
	if _, err := r.token(context.Background(), `Bearer realm="http://169.254.169.254/token"`, "a/b"); err == nil {
		t.Error("a plain-HTTP realm was called")
	}
	if _, err := r.token(context.Background(), `Basic realm="registry"`, "a/b"); err == nil {
		t.Error("a registry that wants a password got an anonymous try")
	}
}
