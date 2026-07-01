package api

import (
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

func testTemplate() *models.Template {
	return &models.Template{
		Variables: []models.TemplateVariable{
			{EnvVariable: "VERSION", Type: models.VarEnum, Options: []string{"1.20", "1.21", "1.22"}, Default: "1.20", Editable: true},
			{EnvVariable: "RCON_PASS", Default: "", Editable: true, Secret: true},
			{EnvVariable: "EULA", Default: "false", Required: true, Editable: true},
			{EnvVariable: "BUILD", Default: "latest", Editable: false},
		},
	}
}

func TestSanitizePorts(t *testing.T) {
	// Defaults: blank protocol -> TCP, generated name, first becomes primary.
	out, err := sanitizePorts([]models.PortSpec{{Port: 25565}, {Port: 25575, Protocol: "udp"}})
	if err != nil {
		t.Fatalf("sanitizePorts: %v", err)
	}
	if len(out) != 2 || out[0].Protocol != "TCP" || out[1].Protocol != "UDP" {
		t.Fatalf("protocols = %+v", out)
	}
	if !out[0].Primary || out[1].Primary {
		t.Errorf("first port should default to primary: %+v", out)
	}
	if out[0].Name == "" {
		t.Error("blank name should be generated")
	}
	// Rejections.
	bad := [][]models.PortSpec{
		{{Port: 0}},                       // out of range
		{{Port: 70000}},                   // out of range
		{{Port: 25565, Protocol: "sctp"}}, // bad protocol
		{{Port: 25565}, {Port: 25565}},    // duplicate
		{{Port: 25565, Primary: true}, {Port: 25575, Primary: true}}, // two primaries
	}
	for i, in := range bad {
		if _, err := sanitizePorts(in); err == nil {
			t.Errorf("case %d: expected error for %+v", i, in)
		}
	}
}

func TestSanitizePortsSharedPortDualProtocol(t *testing.T) {
	// The same port number on TCP and UDP (e.g. Minecraft game + query) is valid
	// and gets protocol-suffixed names so the Kubernetes Service stays valid.
	out, err := sanitizePorts([]models.PortSpec{
		{Port: 25565, Protocol: "TCP"},
		{Port: 25565, Protocol: "UDP"},
	})
	if err != nil {
		t.Fatalf("dual-protocol same port rejected: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 ports, got %d", len(out))
	}
	if out[0].Name == out[1].Name {
		t.Fatalf("names must be unique, both %q", out[0].Name)
	}
	if out[0].Name != "p25565-tcp" || out[1].Name != "p25565-udp" {
		t.Fatalf("unexpected names: %q, %q", out[0].Name, out[1].Name)
	}

	// A single-protocol port keeps the bare name (no needless rename that would
	// reallocate an existing server's node port).
	single, err := sanitizePorts([]models.PortSpec{{Port: 25565, Protocol: "TCP"}})
	if err != nil {
		t.Fatalf("single port: %v", err)
	}
	if single[0].Name != "p25565" {
		t.Fatalf("single-protocol name = %q, want p25565", single[0].Name)
	}

	// Explicit duplicate names are rejected rather than producing an invalid Service.
	if _, err := sanitizePorts([]models.PortSpec{
		{Port: 25565, Protocol: "TCP", Name: "dup"},
		{Port: 25575, Protocol: "UDP", Name: "dup"},
	}); err == nil {
		t.Fatal("expected error for duplicate explicit port names")
	}
}

func TestValidateResources(t *testing.T) {
	ok := []models.Resources{
		{},                          // blank = unlimited
		{Memory: "4Gi"},             // proper unit
		{Memory: "512Mi", CPU: "1"}, // 1 core
		{Memory: "4Mi"},             // exactly the floor
		{CPU: "500m"},
	}
	for _, r := range ok {
		if err := validateResources(r); err != nil {
			t.Errorf("validateResources(%+v) = %v, want nil", r, err)
		}
	}
	bad := []models.Resources{
		{Memory: "4"},   // 4 bytes — the forgotten-unit footgun
		{Memory: "512"}, // 512 bytes
		{Memory: "abc"}, // unparseable
		{CPU: "-1"},     // negative
		{Memory: "1Mi"}, // under the 4Mi floor
	}
	for _, r := range bad {
		if err := validateResources(r); err == nil {
			t.Errorf("validateResources(%+v) = nil, want error", r)
		}
	}
}

func TestResolveEnvToleratesNonEditableAtDefault(t *testing.T) {
	tmpl := testTemplate()
	// A client (e.g. the create form) that echoes a non-editable variable back
	// at its template default must not be rejected — it's a no-op.
	got, err := resolveEnv(tmpl, map[string]string{"EULA": "true", "BUILD": "latest"})
	if err != nil {
		t.Fatalf("non-editable var at its default should be accepted: %v", err)
	}
	if got["BUILD"] != "latest" {
		t.Errorf("BUILD = %q, want latest", got["BUILD"])
	}
	// But an actual attempt to change a non-editable variable is still rejected.
	if _, err := resolveEnv(tmpl, map[string]string{"EULA": "true", "BUILD": "999"}); err == nil {
		t.Error("changing a non-editable var should error")
	}
}

func TestResolveEnvUpdatePreservesSecretsAndCurrent(t *testing.T) {
	tmpl := testTemplate()
	current := map[string]string{"VERSION": "1.21", "RCON_PASS": "oldsecret", "EULA": "true", "BUILD": "123"}

	// Change VERSION, leave RCON_PASS blank (must keep old), don't mention EULA.
	got, err := resolveEnvUpdate(tmpl, current, map[string]string{"VERSION": "1.22", "RCON_PASS": ""})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["VERSION"] != "1.22" {
		t.Errorf("VERSION = %q, want 1.22", got["VERSION"])
	}
	if got["RCON_PASS"] != "oldsecret" {
		t.Errorf("blank secret should be preserved, got %q", got["RCON_PASS"])
	}
	if got["EULA"] != "true" {
		t.Errorf("unspecified var should keep current, EULA = %q", got["EULA"])
	}
	if got["BUILD"] != "123" {
		t.Errorf("non-editable keeps current value, BUILD = %q", got["BUILD"])
	}
}

func TestResolveEnvUpdateRejectsBadInput(t *testing.T) {
	tmpl := testTemplate()
	cur := map[string]string{"EULA": "true"}
	cases := map[string]map[string]string{
		"unknown variable": {"LD_PRELOAD": "/evil.so"},
		"non-editable":     {"BUILD": "999"},
		"bad enum":         {"VERSION": "9.99"},
	}
	for name, req := range cases {
		if _, err := resolveEnvUpdate(tmpl, cur, req); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	// A required variable cleared to empty is rejected.
	if _, err := resolveEnvUpdate(tmpl, map[string]string{}, map[string]string{"EULA": ""}); err == nil {
		t.Error("required empty EULA should error")
	}
}
