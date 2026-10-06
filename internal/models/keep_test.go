package models

import (
	"reflect"
	"strings"
	"testing"
)

func TestCleanKeepPathsTidies(t *testing.T) {
	got, err := CleanKeepPaths([]string{
		"world*", " server.properties ", "", "./ops.json", "config/a.toml",
		"plugins/", "world*", "My World", "config//sub/x.toml",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"world*", "server.properties", "ops.json", "config/a.toml", "plugins", "My World", "config/sub/x.toml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got, err := CleanKeepPaths([]string{" ", ""}); err != nil || got != nil {
		t.Errorf("blank list = %q %v, want nil", got, err)
	}
}

func TestCleanKeepPathsRefuses(t *testing.T) {
	for _, p := range []string{
		"/etc/passwd",     // absolute
		"../other-server", // leaves the volume
		"world/../../x",   // leaves it on the way
		".",               // all of it
		"./",              // all of it, spelt otherwise
		"a\nb",            // would split into two paths
		"a\x00b",          // not a path
		"[unterminated",   // malformed pattern
		strings.Repeat("x", MaxKeepPathLen+1),
	} {
		if _, err := CleanKeepPaths([]string{p}); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	many := make([]string, MaxKeepPaths+1)
	for i := range many {
		many[i] = "p" + strings.Repeat("x", i)
	}
	if _, err := CleanKeepPaths(many); err == nil {
		t.Errorf("%d paths accepted", len(many))
	}
}

func TestEffectiveReinstallKeep(t *testing.T) {
	mc := &Template{Features: []string{"eula", "java_version"}}
	if got := mc.EffectiveReinstallKeep(); !reflect.DeepEqual(got, MinecraftJavaKeep) {
		t.Errorf("Minecraft Java default = %q", got)
	}
	// What a modpack ships is not kept: that is the point of a clean reinstall.
	for _, shipped := range []string{"mods", "config", "kubejs", "defaultconfigs", "libraries"} {
		for _, k := range MinecraftJavaKeep {
			if k == shipped {
				t.Errorf("the Minecraft default keeps %q, which a modpack ships", shipped)
			}
		}
	}
	// The returned list is the caller's: changing it does not change the default.
	got := mc.EffectiveReinstallKeep()
	got[0] = "changed"
	if MinecraftJavaKeep[0] == "changed" {
		t.Error("EffectiveReinstallKeep handed out the shared default")
	}

	own := &Template{Features: []string{"eula"}, ReinstallKeep: []string{"worlds", "allowlist.json"}}
	if got := own.EffectiveReinstallKeep(); !reflect.DeepEqual(got, []string{"worlds", "allowlist.json"}) {
		t.Errorf("a template's own list = %q", got)
	}
	if got := (&Template{}).EffectiveReinstallKeep(); got != nil {
		t.Errorf("another game's default = %q, want none", got)
	}
}
