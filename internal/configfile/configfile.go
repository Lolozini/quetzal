// Package configfile renders egg "config.files" into a server's data volume at
// startup: for each declared file it sets the requested keys (create-or-patch),
// matching what Pterodactyl's Wings does. It runs inside an init container (see
// cmd/configrender) so the parsing happens in Go, independent of the game image.
//
// Find values are templates the controller has already reduced to shell form
// (e.g. "${RCON_PASSWORD}", or a literal port), expanded here against the
// container's environment — so secret values stay in the Kubernetes Secret and
// are never baked into the pod spec.
package configfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is one declared config file.
type Spec struct {
	Path   string            `json:"path"`
	Parser string            `json:"parser"`
	Find   map[string]string `json:"find"`
}

// Render applies every spec under root. getenv resolves ${VAR} references in
// values (pass os.Getenv). A single file's failure is returned but does not stop
// the others; the first error is returned after attempting all.
func Render(root string, specs []Spec, getenv func(string) string) error {
	var firstErr error
	for _, sp := range specs {
		if err := renderOne(root, sp, getenv); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func renderOne(root string, sp Spec, getenv func(string) string) error {
	full := safeJoin(root, sp.Path)
	// Expand ${VAR} in each value against the environment; env values are inserted
	// literally (os.Expand does not recurse), so a value containing '$' is safe.
	vals := make(map[string]string, len(sp.Find))
	for k, tmpl := range sp.Find {
		vals[k] = os.Expand(tmpl, getenv)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("%s: %w", sp.Path, err)
	}
	switch strings.ToLower(sp.Parser) {
	case "properties":
		return applyLineKV(full, vals, '=', false)
	case "ini":
		return applyINI(full, vals)
	case "json":
		return applyStructured(full, vals, marshalJSON, unmarshalJSON)
	case "yaml", "yml":
		return applyStructured(full, vals, marshalYAML, unmarshalYAML)
	case "file":
		return applyFile(full, vals)
	case "xml":
		return fmt.Errorf("%s: xml parser not supported", sp.Path)
	default:
		// Unknown parser: treat like a flat key=value file (best effort).
		return applyLineKV(full, vals, '=', false)
	}
}

// safeJoin confines p under root (".." can never escape).
func safeJoin(root, p string) string {
	return filepath.Join(root, filepath.Clean("/"+p))
}

func readFileOrEmpty(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// ---- properties / flat key=value ----

// applyLineKV patches a line-oriented "key<sep>value" file, preserving existing
// lines/comments/order and appending any keys not already present.
func applyLineKV(path string, vals map[string]string, sep byte, spaced bool) error {
	lines := splitLines(string(readFileOrEmpty(path)))
	applied := make(map[string]bool, len(vals))
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "!") || strings.HasPrefix(t, ";") {
			continue
		}
		j := strings.IndexByte(line, sep)
		if j < 0 {
			continue
		}
		key := strings.TrimSpace(line[:j])
		if v, ok := vals[key]; ok {
			lines[i] = formatKV(key, v, sep, spaced)
			applied[key] = true
		}
	}
	for _, k := range sortedUnapplied(vals, applied) {
		lines = append(lines, formatKV(k, vals[k], sep, spaced))
	}
	return writeFile(path, []byte(joinLines(lines)))
}

func formatKV(k, v string, sep byte, spaced bool) string {
	if spaced {
		return fmt.Sprintf("%s %c %s", k, sep, v)
	}
	return fmt.Sprintf("%s%c%s", k, sep, v)
}

// ---- INI (sections; keys may be "section.key" or top-level "key") ----

func applyINI(path string, vals map[string]string) error {
	lines := splitLines(string(readFileOrEmpty(path)))
	applied := make(map[string]bool, len(vals))
	current := "" // section name, "" = top-level

	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			current = strings.TrimSpace(t[1 : len(t)-1])
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		j := strings.IndexByte(line, '=')
		if j < 0 {
			continue
		}
		key := strings.TrimSpace(line[:j])
		full := key
		if current != "" {
			full = current + "." + key
		}
		if v, ok := vals[full]; ok {
			lines[i] = fmt.Sprintf("%s=%s", key, v)
			applied[full] = true
		}
	}

	// Append the rest, grouped by section.
	bySection := map[string][]string{}
	for _, k := range sortedUnapplied(vals, applied) {
		sec, key := splitSection(k)
		bySection[sec] = append(bySection[sec], fmt.Sprintf("%s=%s", key, vals[k]))
	}
	// Top-level keys first, then named sections (deterministic).
	if rest, ok := bySection[""]; ok {
		lines = append(lines, rest...)
	}
	secs := make([]string, 0, len(bySection))
	for s := range bySection {
		if s != "" {
			secs = append(secs, s)
		}
	}
	sort.Strings(secs)
	for _, s := range secs {
		lines = append(lines, "["+s+"]")
		lines = append(lines, bySection[s]...)
	}
	return writeFile(path, []byte(joinLines(lines)))
}

func splitSection(dotted string) (section, key string) {
	if i := strings.IndexByte(dotted, '.'); i >= 0 {
		return dotted[:i], dotted[i+1:]
	}
	return "", dotted
}

// ---- structured (JSON / YAML), nested dot-keys with type coercion ----

type (
	marshalFn   func(map[string]any) ([]byte, error)
	unmarshalFn func([]byte, *map[string]any) error
)

func applyStructured(path string, vals map[string]string, marshal marshalFn, unmarshal unmarshalFn) error {
	doc := map[string]any{}
	if b := bytes.TrimSpace(readFileOrEmpty(path)); len(b) > 0 {
		_ = unmarshal(b, &doc) // tolerate an unparsable existing file: start fresh
		if doc == nil {
			doc = map[string]any{}
		}
	}
	// Apply in sorted key order so shorter paths don't clobber nested ones set later.
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		setNested(doc, strings.Split(k, "."), coerce(vals[k]))
	}
	out, err := marshal(doc)
	if err != nil {
		return err
	}
	return writeFile(path, out)
}

// setNested sets value at the dotted path, creating intermediate maps. If an
// intermediate node exists but is not a map, it is replaced.
func setNested(m map[string]any, path []string, value any) {
	for i := 0; i < len(path)-1; i++ {
		next, ok := m[path[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[path[i]] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

// coerce turns a string into a bool/int/float when it cleanly looks like one,
// otherwise leaves it as a string (so config types match expectations).
func coerce(s string) any {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func marshalJSON(m map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
func unmarshalJSON(b []byte, m *map[string]any) error { return json.Unmarshal(b, m) }

func marshalYAML(m map[string]any) ([]byte, error)    { return yaml.Marshal(m) }
func unmarshalYAML(b []byte, m *map[string]any) error { return yaml.Unmarshal(b, m) }

// ---- file (line find/replace) ----

// applyFile replaces the first line whose trimmed text starts with a key with
// that key's value; keys not found are appended. Approximates Pterodactyl's
// "file" parser for simple line-based configs.
func applyFile(path string, vals map[string]string) error {
	lines := splitLines(string(readFileOrEmpty(path)))
	applied := make(map[string]bool, len(vals))
	for i, line := range lines {
		t := strings.TrimSpace(line)
		for k, v := range vals {
			if !applied[k] && strings.HasPrefix(t, k) {
				lines[i] = v
				applied[k] = true
				break
			}
		}
	}
	for _, k := range sortedUnapplied(vals, applied) {
		lines = append(lines, vals[k])
	}
	return writeFile(path, []byte(joinLines(lines)))
}

// ---- line helpers ----

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func sortedUnapplied(vals map[string]string, applied map[string]bool) []string {
	var rest []string
	for k := range vals {
		if !applied[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return rest
}
