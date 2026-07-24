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

func TestRawFileURLNormalizesBlobPages(t *testing.T) {
	cases := []struct{ in, want string }{
		// A GitHub blob page serves HTML; the raw host serves the JSON.
		{
			"https://github.com/pterodactyl/game-eggs/blob/master/minecraft/java/paper/egg-paper.json",
			"https://raw.githubusercontent.com/pterodactyl/game-eggs/master/minecraft/java/paper/egg-paper.json",
		},
		// Query strings (?plain=1) are dropped.
		{
			"https://github.com/o/r/blob/main/a/b.json?plain=1",
			"https://raw.githubusercontent.com/o/r/main/a/b.json",
		},
		// GitLab blob -> raw.
		{
			"https://gitlab.com/group/proj/-/blob/main/eggs/x.json",
			"https://gitlab.com/group/proj/-/raw/main/eggs/x.json",
		},
		// Already-raw and unrelated URLs are untouched.
		{
			"https://raw.githubusercontent.com/o/r/main/e.json",
			"https://raw.githubusercontent.com/o/r/main/e.json",
		},
		{"https://eggs.example/e.json", "https://eggs.example/e.json"},
		{"not a url", "not a url"},
	}
	for _, c := range cases {
		if got := rawFileURL(c.in); got != c.want {
			t.Errorf("rawFileURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestImportEggURLFetchesRawForBlobPage(t *testing.T) {
	s := eggTestServer(t)
	var fetched string
	s.Fetch = func(_ context.Context, url string, _ int64) ([]byte, error) {
		fetched = url
		return []byte(eggJSON), nil
	}
	rr := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/templates/import-url",
		strings.NewReader(`{"url":"https://github.com/o/r/blob/main/egg.json"}`)), admin)
	s.handleImportEggURL(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if fetched != "https://raw.githubusercontent.com/o/r/main/egg.json" {
		t.Errorf("fetched %q, want the raw URL", fetched)
	}
}

func TestImportEggHTMLGivesActionableError(t *testing.T) {
	s := eggTestServer(t)
	s.Fetch = func(_ context.Context, _ string, _ int64) ([]byte, error) {
		return []byte("<!DOCTYPE html>\n<html><body>not an egg</body></html>"), nil
	}
	rr := httptest.NewRecorder()
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/templates/import-url",
		strings.NewReader(`{"url":"https://example.test/page"}`)), admin)
	s.handleImportEggURL(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "web page") {
		t.Errorf("error should explain the page/raw mistake: %s", rr.Body.String())
	}
}
