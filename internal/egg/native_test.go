package egg

import (
	"encoding/json"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
)

const minimalEgg = `{
  "name": "Probe Egg",
  "author": "a@b.c",
  "docker_images": {"alpine": "alpine:3.20"},
  "startup": "./run",
  "config": {"files": "{}", "startup": "{\"done\":\"ready\"}", "stop": "^C"},
  "scripts": {"installation": {"script": "#!/bin/sh\necho hi\n", "container": "alpine:3.20", "entrypoint": "sh"}},
  "variables": [
    {"name": "Jar", "env_variable": "SERVER_JARFILE", "default_value": "server.jar",
     "user_viewable": true, "user_editable": true, "rules": "required|string"}
  ]
}`

// An egg still imports as an egg: the dual-format sniff must not change the
// path that carries almost every import.
func TestParseStillReadsAnEgg(t *testing.T) {
	tmpl, err := Parse([]byte(minimalEgg))
	if err != nil {
		t.Fatalf("egg: %v", err)
	}
	if len(tmpl.Images) != 1 || tmpl.Images[0].Ref != "alpine:3.20" {
		t.Errorf("images = %+v", tmpl.Images)
	}
	if tmpl.Install == nil || tmpl.Install.Script == "" {
		t.Error("the install script was dropped")
	}
	if len(tmpl.Variables) != 1 || tmpl.Variables[0].EnvVariable != "SERVER_JARFILE" {
		t.Errorf("variables = %+v", tmpl.Variables)
	}
}

// The bug this closes: Quetzal's own export fed to the import endpoint parsed as
// an egg and "succeeded", producing a template with no images, no install script
// and every variable's env name blank — the first symptom arriving much later as
// `variable "" is required` when someone tried to create a server.
func TestExportedTemplateReimportsIntact(t *testing.T) {
	original, err := Parse([]byte(minimalEgg))
	if err != nil {
		t.Fatal(err)
	}
	// Export is a plain marshal of the stored row, ids and timestamps included.
	original.ID = 42
	original.Version = 7
	exported, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	back, err := Parse(exported)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if len(back.Images) != 1 || back.Images[0].Ref != "alpine:3.20" {
		t.Errorf("images lost on re-import: %+v", back.Images)
	}
	if back.Install == nil || back.Install.Script != original.Install.Script {
		t.Error("install script lost on re-import")
	}
	if len(back.Variables) != 1 {
		t.Fatalf("variables = %d, want 1", len(back.Variables))
	}
	v := back.Variables[0]
	if v.EnvVariable != "SERVER_JARFILE" {
		t.Errorf("envVariable = %q, want SERVER_JARFILE — this is the field that used to come back blank", v.EnvVariable)
	}
	if v.Default != "server.jar" || !v.Editable || !v.Viewable {
		t.Errorf("variable flags lost: %+v", v)
	}
	if back.Startup != original.Startup || back.DataPath != original.DataPath {
		t.Errorf("startup/dataPath drifted: %q %q", back.Startup, back.DataPath)
	}
	// Identity must not ride along: inserting over whatever row holds id 42 here
	// would be someone else's template.
	if back.ID != 0 || back.Version != 0 {
		t.Errorf("id/version carried over: %d/%d", back.ID, back.Version)
	}
}

func TestNativeImportRejections(t *testing.T) {
	cases := map[string]string{
		"no name":       `{"dataPath":"/data","images":[{"ref":"x"}]}`,
		"no images":     `{"name":"x","dataPath":"/data","console":{"type":"attach"}}`,
		"bad dataPath":  `{"name":"x","dataPath":"relative/path","images":[{"ref":"x"}]}`,
		"root dataPath": `{"name":"x","dataPath":"/","images":[{"ref":"x"}]}`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: should have been refused", name)
		}
	}
}

// A native document without a slug gets one from its name, so a hand-written
// template imports like an egg does.
func TestNativeImportDerivesASlug(t *testing.T) {
	tmpl, err := Parse([]byte(`{"name":"My Custom Thing","dataPath":"/data","images":[{"ref":"alpine:3.20","default":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Slug != "my-custom-thing" {
		t.Errorf("slug = %q, want my-custom-thing", tmpl.Slug)
	}
	if tmpl.Console.Type != models.ConsoleAttach {
		t.Errorf("console type = %q, want a default of attach", tmpl.Console.Type)
	}
}
