package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config a game reads must be, at every instant, either the old one or the
// new one -- never a truncated one. Every parser here rewrites the whole file
// from what it read, on every start, from an init container that can be killed
// at any moment (an eviction, a node under memory pressure). Written in place,
// a kill between the truncate and the last byte left the player's
// server.properties empty or cut short.
//
// A hard link to the original is the witness: it shares the original's inode,
// so it shows whatever happens to that inode. If the new content lands through a
// fresh file renamed over the path, the link still reads the old content; if the
// file was truncated and rewritten in place, the link shows that too.
func TestRenderNeverRewritesTheLiveFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(path, []byte("motd=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(dir, "witness")
	if err := os.Link(path, witness); err != nil {
		t.Skipf("no hard links here: %v", err)
	}

	if err := Render(dir, []Spec{{Path: "server.properties", Parser: "properties",
		Find: map[string]string{"motd": "new"}}}, os.Getenv); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "motd=new") {
		t.Fatalf("render did not apply: %q", got)
	}
	old, _ := os.ReadFile(witness)
	if string(old) != "motd=old\n" {
		t.Fatalf("the original file was rewritten in place (witness now %q): a kill mid-write truncates the live config", old)
	}
}

// Replacing the file must not quietly change who can read it: a config holding
// an RCON password is often 0600 on purpose.
func TestRenderKeepsTheFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rcon.properties")
	if err := os.WriteFile(path, []byte("rcon.password=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Render(dir, []Spec{{Path: "rcon.properties", Parser: "properties",
		Find: map[string]string{"rcon.password": "y"}}}, os.Getenv); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A new file still gets the ordinary mode.
func TestRenderCreatesWithDefaultMode(t *testing.T) {
	dir := t.TempDir()
	if err := Render(dir, []Spec{{Path: "new.properties", Parser: "properties",
		Find: map[string]string{"a": "b"}}}, os.Getenv); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "new.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// Nothing of the mechanism is left behind in the player's volume -- including a
// temporary file stranded by an earlier render that was killed mid-write.
func TestRenderLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".server.properties.quetzal-render-123")
	if err := os.WriteFile(stale, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Render(dir, []Spec{{Path: "server.properties", Parser: "properties",
		Find: map[string]string{"a": "b"}}}, os.Getenv); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "quetzal-render") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// A symlinked config was written through the link, to its target. Renaming over
// the path would replace the link with a copy and silently detach whatever the
// link was for (a config shared between two worlds, say), so that case keeps
// the old behaviour.
func TestRenderWritesThroughASymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.properties")
	if err := os.WriteFile(target, []byte("a=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared.properties", filepath.Join(dir, "server.properties")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := Render(dir, []Spec{{Path: "server.properties", Parser: "properties",
		Find: map[string]string{"a": "new"}}}, os.Getenv); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "a=new") {
		t.Errorf("the link's target was not updated: %q", got)
	}
}

// A file that is writable in a directory that is not: in place is the only way
// to write it, and it worked before. Atomicity is not worth breaking it for.
func TestRenderFallsBackWhenTheDirectoryIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "config")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "game.properties")
	if err := os.WriteFile(path, []byte("a=old\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	if err := Render(dir, []Spec{{Path: "config/game.properties", Parser: "properties",
		Find: map[string]string{"a": "new"}}}, os.Getenv); err != nil {
		t.Fatalf("a writable file in a read-only directory is no longer rendered: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "a=new") {
		t.Errorf("not applied: %q", got)
	}
}

// The stale-temporary sweep globs on the config's own name, so a name that
// happens to contain pattern characters must match only itself -- not sweep, or
// miss, some other file.
func TestGlobEscapeMatchesOnlyItself(t *testing.T) {
	for _, name := range []string{"a[1].properties", "we*rd?.yml", `back\slash.ini`} {
		ok, err := filepath.Match(globEscape(name), name)
		if err != nil || !ok {
			t.Errorf("%q does not match itself once escaped (%v)", name, err)
		}
	}
	if ok, _ := filepath.Match(globEscape("a[1].properties"), "a1.properties"); ok {
		t.Error("an escaped bracket still acts as a character class")
	}
}
