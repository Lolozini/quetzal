package api_test

import (
	"net/http"
	"testing"

	"github.com/lolozini/quetzal/internal/crypto"
	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
)

func TestWakeEndpoint(t *testing.T) {
	srv, _, st := newTestServerStore(t)

	// A hibernated, wake-on-connect server.
	s := &models.Server{
		Slug: "sleepy", Namespace: reconciler.NamespaceFor("sleepy"), DesiredState: models.StateRunning,
		Hibernated:  true,
		Hibernation: models.Hibernation{Enabled: true, WakeOnConnect: true},
	}
	if err := st.CreateServer(s); err != nil {
		t.Fatalf("create: %v", err)
	}

	noCookie := &http.Client{}
	wake := func(slug, token string) int {
		r := post(t, noCookie, srv.URL+"/api/internal/wake", map[string]string{"slug": slug, "token": token})
		return r.StatusCode
	}

	// Valid token wakes the server.
	if code := wake("sleepy", crypto.WakeToken(nil, "sleepy")); code != http.StatusNoContent {
		t.Fatalf("valid wake = %d, want 204", code)
	}
	if got, _ := st.GetServer(s.ID); got.Hibernated {
		t.Errorf("server should be woken")
	}

	// Re-hibernate and try a bad token: must be a no-op AND indistinguishable
	// from an unknown slug (both 204), so existence can't be probed.
	if err := st.SetHibernated(s.ID, true); err != nil {
		t.Fatalf("re-hibernate: %v", err)
	}
	if code := wake("sleepy", "wrong-token"); code != http.StatusNoContent {
		t.Errorf("bad token = %d, want 204 (no leak)", code)
	}
	if got, _ := st.GetServer(s.ID); !got.Hibernated {
		t.Errorf("bad token must not wake the server")
	}
	if code := wake("does-not-exist", "whatever"); code != http.StatusNoContent {
		t.Errorf("unknown slug = %d, want 204 (no leak)", code)
	}

	// Proxy mode sets WakeOnConnect=false but must still wake on a valid token.
	p := &models.Server{
		Slug: "proxysrv", Namespace: reconciler.NamespaceFor("proxysrv"), DesiredState: models.StateRunning,
		Hibernated:  true,
		Hibernation: models.Hibernation{Enabled: true, Proxy: true, WakeOnConnect: false},
	}
	if err := st.CreateServer(p); err != nil {
		t.Fatalf("create proxy server: %v", err)
	}
	if code := wake("proxysrv", crypto.WakeToken(nil, "proxysrv")); code != http.StatusNoContent {
		t.Fatalf("proxy wake = %d, want 204", code)
	}
	if got, _ := st.GetServer(p.ID); got.Hibernated {
		t.Errorf("proxy-mode server should wake on a valid token")
	}
}
