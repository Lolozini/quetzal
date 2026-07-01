package egg

import (
	"strings"
	"testing"
)

// TestToTemplateNormalizesCRLF guards against Pterodactyl panel egg exports that
// carry Windows line endings: a stray \r breaks POSIX shells when the install
// script runs (e.g. `then\r` is not the `then` keyword).
func TestToTemplateNormalizesCRLF(t *testing.T) {
	crlf := "{\n" +
		`  "name": "CRLF Egg",` + "\n" +
		`  "docker_images": { "x": "img:tag" },` + "\n" +
		`  "startup": "run\r\n--flag",` + "\n" +
		`  "scripts": { "installation": { "script": "#!/bin/ash\r\nif [ -n \"$X\" ]; then\r\n echo hi\r\nfi\r\n", "container": "alpine", "entrypoint": "ash" } }` + "\n" +
		"}"
	tmpl, err := ToTemplate([]byte(crlf))
	if err != nil {
		t.Fatalf("ToTemplate: %v", err)
	}
	if strings.Contains(tmpl.Install.Script, "\r") {
		t.Errorf("install script still contains CR: %q", tmpl.Install.Script)
	}
	if strings.Contains(tmpl.Startup, "\r") {
		t.Errorf("startup still contains CR: %q", tmpl.Startup)
	}
}

const sampleEgg = `{
  "name": "Paper Test",
  "author": "a@b.c",
  "description": "test egg",
  "features": ["eula"],
  "docker_images": { "Java 21": "itzg/minecraft-server:java21" },
  "startup": "java -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}",
  "config": {
    "files": "{\"server.properties\":{\"parser\":\"properties\",\"find\":{\"server-port\":\"{{server.build.default.port}}\"}}}",
    "startup": "{\"done\":\")! For help, type \"}",
    "logs": "{}",
    "stop": "stop"
  },
  "scripts": {
    "installation": {
      "script": "#!/bin/bash\necho install",
      "container": "ghcr.io/pterodactyl/installers:debian",
      "entrypoint": "bash"
    }
  },
  "variables": [
    { "name": "Jar File", "env_variable": "SERVER_JARFILE", "default_value": "server.jar", "user_viewable": true, "user_editable": true, "rules": "required|string", "field_type": "text" },
    { "name": "Version", "env_variable": "MC_VERSION", "default_value": "latest", "user_viewable": true, "user_editable": true, "rules": "required|in:latest,1.20,1.21", "field_type": "select" }
  ]
}`

func TestToTemplate(t *testing.T) {
	tmpl, err := ToTemplate([]byte(sampleEgg))
	if err != nil {
		t.Fatalf("ToTemplate: %v", err)
	}

	if tmpl.Slug != "paper-test" {
		t.Errorf("slug = %q, want paper-test", tmpl.Slug)
	}
	// Imported eggs mount their data at /home/container (Pterodactyl's server dir),
	// which many eggs hardcode in config.files / resolve files against.
	if tmpl.DataPath != "/home/container" {
		t.Errorf("dataPath = %q, want /home/container", tmpl.DataPath)
	}
	if tmpl.StopCommand != "stop" {
		t.Errorf("stopCommand = %q, want stop", tmpl.StopCommand)
	}
	if tmpl.DoneRegex != ")! For help, type " {
		t.Errorf("doneRegex = %q", tmpl.DoneRegex)
	}
	if len(tmpl.Images) != 1 || tmpl.Images[0].Ref != "itzg/minecraft-server:java21" {
		t.Errorf("images = %+v", tmpl.Images)
	}
	if !tmpl.Images[0].Default {
		t.Errorf("first image should be default")
	}
	if len(tmpl.Features) != 1 || tmpl.Features[0] != "eula" {
		t.Errorf("features = %+v", tmpl.Features)
	}
	if tmpl.Install == nil || tmpl.Install.Image != "ghcr.io/pterodactyl/installers:debian" {
		t.Errorf("install = %+v", tmpl.Install)
	}
	if len(tmpl.ConfigFiles) != 1 {
		t.Fatalf("configFiles = %+v", tmpl.ConfigFiles)
	}
	if tmpl.ConfigFiles[0].Path != "server.properties" || tmpl.ConfigFiles[0].Parser != "properties" {
		t.Errorf("configFile = %+v", tmpl.ConfigFiles[0])
	}

	if len(tmpl.Variables) != 2 {
		t.Fatalf("variables = %d, want 2", len(tmpl.Variables))
	}
	enum := tmpl.Variables[1]
	if enum.Type != "enum" {
		t.Errorf("var type = %q, want enum", enum.Type)
	}
	wantOpts := []string{"latest", "1.20", "1.21"}
	if len(enum.Options) != len(wantOpts) {
		t.Fatalf("options = %+v", enum.Options)
	}
	for i, o := range wantOpts {
		if enum.Options[i] != o {
			t.Errorf("option[%d] = %q, want %q", i, enum.Options[i], o)
		}
	}
	if !tmpl.Variables[0].Required {
		t.Errorf("var 0 should be required")
	}
}
