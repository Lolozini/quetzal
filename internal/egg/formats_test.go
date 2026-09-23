package egg

import (
	"strings"
	"testing"
)

// A Pelican egg as its repositories publish it (PLCN_v3, YAML), trimmed.
const pelicanYAML = `_comment: 'DO NOT EDIT: FILE GENERATED AUTOMATICALLY BY PANEL'
meta:
  version: PLCN_v3
name: Paper
author: parker@example.com
uuid: 5da37ef6-58da-4169-90a6-e683e1721247
features:
  - eula
  - java_version
docker_images:
  'Java 25': 'ghcr.io/pelican-eggs/yolks:java_25'
  'Java 21': 'ghcr.io/pelican-eggs/yolks:java_21'
  'Java 8': 'ghcr.io/pelican-eggs/yolks:java_8'
file_denylist: {  }
startup_commands:
  Default: 'java -Xms128M -jar {{SERVER_JARFILE}}'
  Alternative: 'java -jar other.jar'
config:
  files:
    server.properties:
      parser: properties
      find:
        server-port: '{{server.allocations.default.port}}'
  startup:
    done: ')! For help, type '
  logs: {  }
  stop: stop
scripts:
  installation:
    script: |-
      #!/bin/bash
      echo installing
    container: 'ghcr.io/pelican-eggs/installers:alpine'
    entrypoint: ash
variables:
  -
    name: 'Server Jar File'
    env_variable: SERVER_JARFILE
    default_value: server.jar
    user_viewable: true
    user_editable: true
    rules:
      - required
      - 'regex:/^([\w\d._-]+)(\.jar)$/'
    sort: 1
  -
    name: 'Max players'
    env_variable: MAX_PLAYERS
    default_value: 20
    user_viewable: true
    user_editable: true
    rules:
      - required
      - integer
  -
    name: 'Mode'
    env_variable: MODE
    default_value: survival
    user_viewable: true
    user_editable: true
    rules: 'required|in:survival,creative'
`

func TestParsePelicanYAML(t *testing.T) {
	tmpl, err := Parse([]byte(pelicanYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tmpl.Name != "Paper" || tmpl.Startup != "java -Xms128M -jar {{SERVER_JARFILE}}" || tmpl.StopCommand != "stop" {
		t.Errorf("name/startup/stop = %q / %q / %q", tmpl.Name, tmpl.Startup, tmpl.StopCommand)
	}
	if tmpl.DoneRegex != ")! For help, type " {
		t.Errorf("done = %q", tmpl.DoneRegex)
	}
	// Images keep the egg's order, and the first is the default.
	var refs []string
	for _, i := range tmpl.Images {
		refs = append(refs, i.Ref)
	}
	if strings.Join(refs, ",") != "ghcr.io/pelican-eggs/yolks:java_25,ghcr.io/pelican-eggs/yolks:java_21,ghcr.io/pelican-eggs/yolks:java_8" ||
		!tmpl.Images[0].Default || tmpl.Images[1].Default {
		t.Errorf("images = %+v", tmpl.Images)
	}
	if len(tmpl.FileDenylist) != 0 || len(tmpl.Features) != 2 {
		t.Errorf("denylist/features = %v / %v", tmpl.FileDenylist, tmpl.Features)
	}
	if tmpl.Install == nil || tmpl.Install.Entrypoint != "ash" || !strings.Contains(tmpl.Install.Script, "echo installing") {
		t.Errorf("install = %+v", tmpl.Install)
	}
	if len(tmpl.ConfigFiles) != 1 || tmpl.ConfigFiles[0].Find["server-port"] != "{{server.allocations.default.port}}" {
		t.Errorf("config files = %+v", tmpl.ConfigFiles)
	}
	if len(tmpl.Variables) != 3 {
		t.Fatalf("variables = %+v", tmpl.Variables)
	}
	jar, players, mode := tmpl.Variables[0], tmpl.Variables[1], tmpl.Variables[2]
	// A rule list reads like the rule string it stands for.
	if !jar.Required || jar.Rules != `required|regex:/^([\w\d._-]+)(\.jar)$/` {
		t.Errorf("jar = %+v", jar)
	}
	// An unquoted number is still a default value.
	if players.Default != "20" || players.Type != "int" {
		t.Errorf("players = %+v", players)
	}
	if mode.Type != "enum" || strings.Join(mode.Options, ",") != "survival,creative" {
		t.Errorf("mode = %+v", mode)
	}
}

func TestDockerImagesKeepTheirOrder(t *testing.T) {
	// Twenty images: with a map, the "first" one was random.
	var b strings.Builder
	b.WriteString(`{"name":"x","startup":"run","docker_images":{`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"img` + string(rune('a'+i)) + `":"ref` + string(rune('a'+i)) + `"`)
	}
	b.WriteString(`}}`)
	for n := 0; n < 5; n++ {
		tmpl, err := Parse([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		if tmpl.Images[0].Ref != "refa" || !tmpl.Images[0].Default || tmpl.Images[19].Ref != "reft" {
			t.Fatalf("images = %+v", tmpl.Images)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "   ", "just some text", "- a\n- b\n", "a: [unclosed"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
}
