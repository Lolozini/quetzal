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
