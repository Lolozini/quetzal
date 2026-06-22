package configfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPropertiesPatchAndAppendAndExpand(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "server.properties", "# header\nmotd=old\nmax-players=20\n")
	specs := []Spec{{
		Path:   "server.properties",
		Parser: "properties",
		Find: map[string]string{
			"motd":          "Hello ${WORLD}",
			"server-port":   "25565", // new key -> appended
			"rcon.password": "${SECRET}",
		},
	}}
	if err := Render(dir, specs, env(map[string]string{"WORLD": "Quetzal", "SECRET": "s3cr3t"})); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "server.properties")
	for _, want := range []string{"# header", "motd=Hello Quetzal", "max-players=20", "server-port=25565", "rcon.password=s3cr3t"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "motd=old") {
		t.Error("old value not replaced")
	}
}

func TestPropertiesCreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{Path: "sub/new.properties", Parser: "properties", Find: map[string]string{"a": "1"}}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "sub/new.properties"); strings.TrimSpace(got) != "a=1" {
		t.Errorf("created file = %q", got)
	}
}

func TestJSONNestedAndCoercion(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.json", `{"server":{"name":"old"},"keep":true}`)
	specs := []Spec{{
		Path:   "config.json",
		Parser: "json",
		Find: map[string]string{
			"server.name":        "New",
			"server.port":        "25565",
			"server.pvp":         "true",
			"server.ratio":       "1.5",
			"server.description": "a string",
		},
	}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, dir, "config.json")), &doc); err != nil {
		t.Fatal(err)
	}
	srv := doc["server"].(map[string]any)
	if srv["name"] != "New" {
		t.Errorf("name = %v", srv["name"])
	}
	if srv["port"].(float64) != 25565 { // JSON numbers decode as float64
		t.Errorf("port not coerced to number: %v (%T)", srv["port"], srv["port"])
	}
	if srv["pvp"] != true {
		t.Errorf("pvp not coerced to bool: %v", srv["pvp"])
	}
	if srv["ratio"].(float64) != 1.5 {
		t.Errorf("ratio = %v", srv["ratio"])
	}
	if srv["description"] != "a string" {
		t.Errorf("description = %v", srv["description"])
	}
	if doc["keep"] != true {
		t.Errorf("existing key not preserved: %v", doc["keep"])
	}
}

func TestYAMLNested(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.yml", "settings:\n  name: old\n  extra: keep\n")
	specs := []Spec{{
		Path:   "config.yml",
		Parser: "yaml",
		Find:   map[string]string{"settings.name": "New", "settings.maxplayers": "100"},
	}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(read(t, dir, "config.yml")), &doc); err != nil {
		t.Fatal(err)
	}
	settings := doc["settings"].(map[string]any)
	if settings["name"] != "New" {
		t.Errorf("name = %v", settings["name"])
	}
	if settings["maxplayers"] != 100 {
		t.Errorf("maxplayers not coerced to int: %v (%T)", settings["maxplayers"], settings["maxplayers"])
	}
	if settings["extra"] != "keep" {
		t.Errorf("existing key lost: %v", settings["extra"])
	}
}

func TestINISections(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "app.ini", "[net]\nport=1\n[other]\nx=y\n")
	specs := []Spec{{
		Path:   "app.ini",
		Parser: "ini",
		Find:   map[string]string{"net.port": "25565", "net.host": "0.0.0.0", "top": "v"},
	}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "app.ini")
	if !strings.Contains(got, "port=25565") {
		t.Errorf("net.port not patched:\n%s", got)
	}
	for _, want := range []string{"host=0.0.0.0", "top=v", "x=y"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFileParserReplacesLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "run.sh", "#!/bin/sh\nJAVA_OPTS=-Xmx1G\necho run\n")
	specs := []Spec{{Path: "run.sh", Parser: "file", Find: map[string]string{"JAVA_OPTS": "JAVA_OPTS=-Xmx4G"}}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	got := read(t, dir, "run.sh")
	if !strings.Contains(got, "JAVA_OPTS=-Xmx4G") || strings.Contains(got, "Xmx1G") {
		t.Errorf("line not replaced:\n%s", got)
	}
}

func TestEnvValueInsertedLiterallyNoRecursion(t *testing.T) {
	// A secret value containing '$' must be inserted verbatim, not re-expanded.
	dir := t.TempDir()
	specs := []Spec{{Path: "x.properties", Parser: "properties", Find: map[string]string{"pw": "${PW}"}}}
	if err := Render(dir, specs, env(map[string]string{"PW": "a$bc${NOPE}", "NOPE": "LEAK"})); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "x.properties"); !strings.Contains(got, "pw=a$bc${NOPE}") {
		t.Errorf("env value was re-expanded: %q", got)
	}
}

func TestRenderConfinesPath(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{Path: "../../escape.txt", Parser: "properties", Find: map[string]string{"a": "1"}}}
	if err := Render(dir, specs, env(nil)); err != nil {
		t.Fatal(err)
	}
	// The file must land under dir, never at the real parent.
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err != nil {
		t.Errorf("expected confined file under root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Error("path traversal escaped the root")
	}
}
