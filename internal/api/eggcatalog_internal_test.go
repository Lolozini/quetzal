package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
	"github.com/lolozini/quetzal/templates"
)

const eggJSON = `{
  "name": "Catalog Egg",
  "author": "a@b.c",
  "docker_images": { "img": "alpine:3.20" },
  "startup": "run"
}`

func eggTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "egg.db"), Silent: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := templates.Seed(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return New(st, fake.NewSimpleClientset(), &rest.Config{})
}

func asUser(r *http.Request, u *models.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
}

var admin = &models.User{ID: 1, Username: "admin", IsAdmin: true}

func TestImportEggURL(t *testing.T) {
	s := eggTestServer(t)
	// Stub the SSRF-guarded fetcher to return a known egg.
	s.Fetch = func(_ context.Context, url string, _ int64) ([]byte, error) {
		if url != "https://eggs.example/catalog-egg.json" {
			t.Errorf("unexpected url %q", url)
		}
		return []byte(eggJSON), nil
	}

	rr := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/templates/import-url",
		strings.NewReader(`{"url":"https://eggs.example/catalog-egg.json"}`)), admin)
	s.handleImportEggURL(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Store.GetTemplateBySlug("catalog-egg"); err != nil {
		t.Fatalf("template not imported: %v", err)
	}
}

func TestImportEggURLRequiresAdmin(t *testing.T) {
	s := eggTestServer(t)
	called := false
	s.Fetch = func(_ context.Context, _ string, _ int64) ([]byte, error) { called = true; return []byte(eggJSON), nil }

	rr := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/templates/import-url",
		strings.NewReader(`{"url":"https://eggs.example/x.json"}`)), &models.User{ID: 2, Username: "bob"})
	s.handleImportEggURL(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rr.Code)
	}
	if called {
		t.Error("fetch must not run for a non-admin")
	}
}

func TestImportEggURLValidation(t *testing.T) {
	s := eggTestServer(t)
	rr := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/templates/import-url", strings.NewReader(`{"url":""}`)), admin)
	s.handleImportEggURL(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty url status = %d, want 400", rr.Code)
	}
}

func TestEggCatalogGetAndSet(t *testing.T) {
	s := eggTestServer(t)

	// No catalog configured -> empty list, no fetch.
	rr := httptest.NewRecorder()
	s.handleGetEggCatalog(rr, asUser(httptest.NewRequest(http.MethodGet, "/api/egg-catalog", nil), admin))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"eggs":[]`) {
		t.Fatalf("empty catalog = %d %s", rr.Code, rr.Body.String())
	}

	// Set a catalog URL.
	rr = httptest.NewRecorder()
	s.handleSetEggCatalog(rr, asUser(httptest.NewRequest(http.MethodPut, "/api/egg-catalog",
		strings.NewReader(`{"url":"https://eggs.example/catalog.json"}`)), admin))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("set catalog = %d", rr.Code)
	}

	// Now GET fetches + parses the manifest (mixed valid/invalid entries).
	s.Fetch = func(_ context.Context, _ string, _ int64) ([]byte, error) {
		return []byte(`[{"name":"A","url":"https://x/a.json"},{"name":"no-url"},{"url":"no-name"}]`), nil
	}
	rr = httptest.NewRecorder()
	s.handleGetEggCatalog(rr, asUser(httptest.NewRequest(http.MethodGet, "/api/egg-catalog", nil), admin))
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("get catalog = %d %s", rr.Code, body)
	}
	if !strings.Contains(body, `"name":"A"`) || strings.Contains(body, "no-url") || strings.Contains(body, "no-name") {
		t.Errorf("catalog should list only complete entries: %s", body)
	}
}

func TestSetEggCatalogRejectsBadScheme(t *testing.T) {
	s := eggTestServer(t)
	rr := httptest.NewRecorder()
	s.handleSetEggCatalog(rr, asUser(httptest.NewRequest(http.MethodPut, "/api/egg-catalog",
		strings.NewReader(`{"url":" file:///etc/passwd"}`)), admin))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad scheme status = %d, want 400", rr.Code)
	}
}

func TestParseCatalogFormats(t *testing.T) {
	// Bare array.
	got, err := parseCatalog([]byte(`[{"name":"A","url":"u1"}]`))
	if err != nil || len(got) != 1 || got[0].Name != "A" {
		t.Fatalf("bare array: %v %+v", err, got)
	}
	// Wrapped {"eggs":[...]}.
	got, err = parseCatalog([]byte(`{"eggs":[{"name":"B","url":"u2"},{"name":"C","url":"u3"}]}`))
	if err != nil || len(got) != 2 {
		t.Fatalf("wrapped: %v %+v", err, got)
	}
	// Garbage.
	if _, err := parseCatalog([]byte(`not json`)); err == nil {
		t.Error("garbage should error")
	}
}
