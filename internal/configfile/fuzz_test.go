package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config path comes from a template, which an admin may have imported from
// anywhere. Whatever it says, the file written must be inside the data volume.
func FuzzSafeJoin(f *testing.F) {
	for _, p := range []string{"server.properties", "../../etc/passwd", "/abs/path", "a/../../b", "", ".", "..", "a/./b//c"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		const root = "/data"
		got := safeJoin(root, p)
		if got != root && !strings.HasPrefix(got, root+"/") {
			t.Fatalf("safeJoin(%q, %q) = %q, outside the root", root, p, got)
		}
	})
}

// The render runs on every start, over the file the previous start wrote. A
// second pass with the same values must leave it as the first pass did, or the
// file drifts a little further at each restart.
func FuzzLineKVIdempotent(f *testing.F) {
	f.Add("motd=A Minecraft Server\nserver-port=25565\n", "server-port", "25566")
	f.Add("# comment\n\nkey = value\n", "key", "new value")
	f.Add("", "difficulty", "hard")
	f.Fuzz(func(t *testing.T, content, key, value string) {
		checkIdempotent(t, content, func(path string) error {
			return applyLineKV(path, map[string]string{key: value}, '=', false)
		})
	})
}

func FuzzINIIdempotent(f *testing.F) {
	f.Add("[server]\nport=2456\n", "server.port", "2457")
	f.Add("top=1\n[a]\nb=2\n", "a.c", "3")
	f.Add("", "name", "Vikings")
	f.Fuzz(func(t *testing.T, content, key, value string) {
		checkIdempotent(t, content, func(path string) error {
			return applyINI(path, map[string]string{key: value})
		})
	})
}

func checkIdempotent(t *testing.T, content string, apply func(path string) error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := apply(path); err != nil {
		return // refused input: nothing written to compare
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(path); err != nil {
		t.Fatalf("second pass failed after the first succeeded: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("not idempotent:\nfirst:  %q\nsecond: %q", first, second)
	}
}
