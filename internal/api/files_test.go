package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJailConfinesPaths(t *testing.T) {
	const root = "/data"
	cases := map[string]string{
		"":                  "/data",
		"/":                 "/data",
		"mods":              "/data/mods",
		"mods/config.yml":   "/data/mods/config.yml",
		"mods/../world":     "/data/world",
		"../../etc/passwd":  "/data/etc/passwd",
		"/etc/passwd":       "/data/etc/passwd",
		"a/b/../../../../x": "/data/x",
		"./././../secret":   "/data/secret",
		"..":                "/data",
		"../":               "/data",
	}
	for in, want := range cases {
		if got := jail(root, in); got != want {
			t.Errorf("jail(%q) = %q, want %q", in, got, want)
		}
	}
	// Property: the result is always within root, whatever the input.
	for _, in := range []string{"../../../etc", "....//....//x", "foo/../../../../../../root"} {
		got := jail(root, in)
		if got != root && !strings.HasPrefix(got, root+"/") {
			t.Errorf("jail(%q) = %q escaped root %q", in, got, root)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := sanitizeFilename("ok.txt"); got != "ok.txt" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeFilename("a\"b\\c\nd"); got != "abcd" {
		t.Errorf("sanitize did not strip unsafe chars: %q", got)
	}
}

// Only a path that climbs above the root is refused; ".." that stays inside it,
// and names that merely start with two dots, are ordinary paths.
func TestOutsideRoot(t *testing.T) {
	for p, want := range map[string]bool{
		"": false, "/": false, "mods": false, "a/../b": false, "world/..": false,
		"..foo": false, "a/..b": false, "/x/../y/": false,
		"..": true, "../": true, "/..": true, "../x": true, "a/../../x": true,
		"//..//x": true, "./../x": true, "a/b/../../..": true,
	} {
		w := httptest.NewRecorder()
		if got := outsideRoot(w, p); got != want {
			t.Errorf("outsideRoot(%q) = %v, want %v", p, got, want)
		} else if want && w.Code != http.StatusBadRequest {
			t.Errorf("outsideRoot(%q) answered %d, want 400", p, w.Code)
		}
	}
	// Every path given is checked, not only the first.
	if w := httptest.NewRecorder(); !outsideRoot(w, "fine", "../not") {
		t.Error("the second path was not checked")
	}
}
