package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/api"
)

func headersFor(t *testing.T, secure bool, path string) http.Header {
	t.Helper()
	s := &api.Server{Secure: secure}
	h := s.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result().Header
}

func TestSecurityHeaders(t *testing.T) {
	h := headersFor(t, false, "/")
	for k, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'",
		"base-uri 'none'", "object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}
	// 'unsafe-inline'/'unsafe-eval' in script-src would give back most of what
	// the policy is for. The UI has no inline script, so this must stay true.
	script := csp[strings.Index(csp, "script-src"):]
	script = script[:strings.Index(script, ";")]
	if strings.Contains(script, "unsafe") {
		t.Errorf("script-src should not allow unsafe sources: %q", script)
	}
}

// HSTS is only asserted where the panel knows it is behind TLS: claiming it from
// an http-only install pins the browser to a scheme that does not answer.
func TestHSTSOnlyWhenSecure(t *testing.T) {
	if got := headersFor(t, false, "/").Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS on an insecure install = %q, want none", got)
	}
	if got := headersFor(t, true, "/").Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
		t.Errorf("HSTS behind TLS = %q, want a max-age", got)
	}
}

// The docs viewer loads Redoc from a CDN, so it relaxes the policy for itself —
// without opening the rest of the panel and without becoming frameable.
func TestDocsPageHasItsOwnPolicy(t *testing.T) {
	srv, c := newTestServer(t)
	resp, err := c.Get(srv.URL + "/api/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "cdn.jsdelivr.net") {
		t.Errorf("docs CSP should allow the Redoc CDN: %q", csp)
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("docs CSP should still refuse framing: %q", csp)
	}
	// The relaxation must not leak to any other page.
	if other := headersFor(t, false, "/").Get("Content-Security-Policy"); strings.Contains(other, "jsdelivr") {
		t.Errorf("panel CSP picked up the docs exception: %q", other)
	}
}
