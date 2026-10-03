package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectPorts(t *testing.T) {
	tmpl := &Template{Variables: []TemplateVariable{
		{EnvVariable: "QUERY_PORT", Default: "27015"},
		{EnvVariable: "RCON_PORT", Default: "25575"},
		{EnvVariable: "STEAMPORT", Default: "8766"},    // PORT suffix without underscore
		{EnvVariable: "MAX_PLAYERS", Default: "20"},    // not a port despite numeric default
		{EnvVariable: "WEB_PORT", Default: ""},         // unset -> skipped
		{EnvVariable: "EXTRA_PORT", Default: "0"},      // disabled -> skipped
		{EnvVariable: "SERVER_PORT", Default: "28015"}, // Wings global -> skipped (it's the primary)
		{EnvVariable: "DUP_PORT", Default: "27015"},    // duplicate of QUERY_PORT -> skipped
	}}
	got := DetectPorts(tmpl)
	// Each port on TCP and on UDP, as Wings exposes an allocation.
	want := []PortSpec{
		{Name: "query", Port: 27015, Protocol: "TCP", Primary: true},
		{Name: "query", Port: 27015, Protocol: "UDP"},
		{Name: "rcon", Port: 25575, Protocol: "TCP"},
		{Name: "rcon", Port: 25575, Protocol: "UDP"},
		{Name: "steam", Port: 8766, Protocol: "TCP"},
		{Name: "steam", Port: 8766, Protocol: "UDP"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Counter-Strike 2's egg gives the game its allocation (-port {{SERVER_PORT}})
// and has one port variable, SourceTV's. Taken for the game's port, it put the
// game on 27020 next to SourceTV, on TCP only, and nobody could join: a port
// variable is never the primary of an egg that uses its allocation.
func TestDetectPortsLeavesTheGamePortToTheAllocation(t *testing.T) {
	cs2 := &Template{
		Startup:   "./game/bin/linuxsteamrt64/cs2 -dedicated -ip 0.0.0.0 -port {{SERVER_PORT}} -tv_port {{TV_PORT}} +map {{SRCDS_MAP}}",
		Variables: []TemplateVariable{{EnvVariable: "SRCDS_MAP", Default: "de_dust2"}, {EnvVariable: "TV_PORT", Default: "27020"}},
	}
	if !cs2.UsesAllocation() {
		t.Fatal("an egg with -port {{SERVER_PORT}} uses its allocation")
	}
	got := DetectPorts(cs2)
	if len(got) != 2 || got[0].Port != 27020 || got[0].Name != "tv" {
		t.Fatalf("got %+v, want SourceTV's port on TCP and UDP", got)
	}
	for _, p := range got {
		if p.Primary {
			t.Errorf("%+v is primary: the game's port is the allocation", p)
		}
	}

	// A Minecraft egg hands it over in its config files instead.
	paper := &Template{
		Startup:     "java -jar {{SERVER_JARFILE}}",
		ConfigFiles: []ConfigFile{{Path: "server.properties", Parser: "properties", Find: map[string]string{"server-port": "{{server.build.default.port}}"}}},
	}
	if !paper.UsesAllocation() {
		t.Error("an egg whose config files take server.build.default.port uses its allocation")
	}
	if (&Template{Startup: "./bot --token {{TOKEN}}"}).UsesAllocation() {
		t.Error("an egg that never mentions its allocation does not use it")
	}
}

func TestDetectPortsNoneWhenNoPortVars(t *testing.T) {
	// A typical Minecraft egg (Paper) declares no port variables -> no suggestions,
	// the editor stays manual.
	tmpl := &Template{Variables: []TemplateVariable{
		{EnvVariable: "SERVER_JARFILE", Default: "server.jar"},
		{EnvVariable: "MINECRAFT_VERSION", Default: "latest"},
	}}
	if got := DetectPorts(tmpl); got != nil {
		t.Errorf("expected no suggested ports, got %+v", got)
	}
}

// A template without variables or images is sent with empty lists: the panel
// reads them as lists, and "variables": null crashed its create form into a
// blank page.
func TestTemplateJSONListsAreNeverNull(t *testing.T) {
	for _, v := range []any{Template{Slug: "bare"}, &Template{Slug: "bare"}, []Template{{Slug: "bare"}}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, `"variables":[]`) || !strings.Contains(s, `"images":[]`) {
			t.Errorf("%T marshals to %s, want empty lists", v, s)
		}
	}
}
