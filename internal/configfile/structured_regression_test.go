package configfile

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJSONPreservesUntouchedNumbers(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.json", `{"id":9007199254740993,"negative":-9223372036854775807,"ratio":0.1234567890123456789,"port":1}`)
	spec := []Spec{{Path: "config.json", Parser: "json", Find: map[string]string{"port": "2"}}}
	for range 2 {
		if err := Render(dir, spec, env(nil)); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewBufferString(read(t, dir, "config.json")))
		dec.UseNumber()
		var got map[string]any
		if err := dec.Decode(&got); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"id": "9007199254740993", "negative": "-9223372036854775807", "ratio": "0.1234567890123456789", "port": "2"} {
			if got[key] != json.Number(want) {
				t.Errorf("%s = %v, want %s", key, got[key], want)
			}
		}
	}
}

func TestInvalidStructuredConfigurationIsPreserved(t *testing.T) {
	for _, tc := range []struct{ name, parser, body string }{
		{"broken JSON", "json", `{"keep":"important",`},
		{"trailing JSON", "json", `{"keep":"important"} {}`},
		{"array JSON", "json", `["important"]`},
		{"broken YAML", "yaml", "keep: important\ninvalid: ["},
		{"array YAML", "yaml", "- important\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "config", tc.body)
			err := Render(dir, []Spec{{Path: "config", Parser: tc.parser, Find: map[string]string{"port": "2"}}}, env(nil))
			if err == nil {
				t.Error("invalid existing configuration accepted")
			}
			if got := read(t, dir, "config"); got != tc.body {
				t.Errorf("configuration overwritten: %q", got)
			}
		})
	}
}
