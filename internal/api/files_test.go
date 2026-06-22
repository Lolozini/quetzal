package api

import (
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
