package egg

import (
	"encoding/json"
	"testing"
)

func TestParseJSONWithBOM(t *testing.T) {
	tmpl, err := Parse(append([]byte{0xef, 0xbb, 0xbf}, []byte(minimalEgg)...))
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.Name != "Probe Egg" || tmpl.Images[0].Ref != "alpine:3.24" {
		t.Fatalf("BOM changed the imported template: %+v", tmpl)
	}
}

func TestNumericVariableDefaultKeepsPrecision(t *testing.T) {
	for _, number := range []string{"9007199254740993", "-9223372036854775807", "0.1234567890123456789"} {
		t.Run(number, func(t *testing.T) {
			doc := `{"name":"Seed","docker_images":{"game":"game:latest"},"variables":[{"env_variable":"SEED","default_value":` + number + `}]}`
			tmpl, err := Parse([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if got := tmpl.Variables[0].Default; got != number {
				t.Fatalf("default = %q, want %q", got, number)
			}
		})
	}
}

func TestNativeImportRejectsNonAbsoluteDataPath(t *testing.T) {
	for _, dataPath := range []string{" /data", "\t/data", "   "} {
		t.Run(dataPath, func(t *testing.T) {
			doc, err := json.Marshal(map[string]any{"name": "Invalid path", "dataPath": dataPath, "images": []map[string]string{{"ref": "alpine:3.24"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(doc); err == nil {
				t.Fatalf("accepted non-absolute mount path %q", dataPath)
			}
		})
	}
}
