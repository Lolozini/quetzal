package models

import "testing"

func TestDetectPorts(t *testing.T) {
	vars := []TemplateVariable{
		{EnvVariable: "QUERY_PORT", Default: "27015"},
		{EnvVariable: "RCON_PORT", Default: "25575"},
		{EnvVariable: "STEAMPORT", Default: "8766"},    // PORT suffix without underscore
		{EnvVariable: "MAX_PLAYERS", Default: "20"},    // not a port despite numeric default
		{EnvVariable: "WEB_PORT", Default: ""},         // unset -> skipped
		{EnvVariable: "EXTRA_PORT", Default: "0"},      // disabled -> skipped
		{EnvVariable: "SERVER_PORT", Default: "28015"}, // Wings global -> skipped (it's the primary)
		{EnvVariable: "DUP_PORT", Default: "27015"},    // duplicate of QUERY_PORT -> skipped
	}
	got := DetectPorts(vars)
	if len(got) != 3 {
		t.Fatalf("expected 3 detected ports, got %d: %+v", len(got), got)
	}
	want := []struct {
		name string
		port int32
	}{{"query", 27015}, {"rcon", 25575}, {"steam", 8766}}
	for i, w := range want {
		if got[i].Port != w.port || got[i].Name != w.name {
			t.Errorf("port[%d] = %+v, want %s/%d", i, got[i], w.name, w.port)
		}
		if got[i].Protocol != "TCP" {
			t.Errorf("port[%d] protocol = %q, want TCP", i, got[i].Protocol)
		}
	}
	// The first detected port defaults to primary; the rest don't.
	if !got[0].Primary || got[1].Primary || got[2].Primary {
		t.Errorf("only the first port should default to primary: %+v", got)
	}
}

func TestDetectPortsNoneWhenNoPortVars(t *testing.T) {
	// A typical Minecraft egg (Paper) declares no port variables -> no suggestions,
	// the editor stays manual.
	vars := []TemplateVariable{
		{EnvVariable: "SERVER_JARFILE", Default: "server.jar"},
		{EnvVariable: "MINECRAFT_VERSION", Default: "latest"},
	}
	if got := DetectPorts(vars); got != nil {
		t.Errorf("expected no suggested ports, got %+v", got)
	}
}
