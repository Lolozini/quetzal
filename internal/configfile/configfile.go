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
	"errors"
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
		return applyXML(full, vals)
	default:
		// Unknown parser: treat like a flat key=value file (best effort).
		return applyLineKV(full, vals, '=', false)
	}
}

// safeJoin confines p under root (".." can never escape).
func safeJoin(root, p string) string {
	return filepath.Join(root, filepath.Clean("/"+p))
}

// readExisting returns a config file's current contents, or nil when it does
// not exist yet (the create case). Any other read error is returned rather than
// reported as an empty file: every parser here rewrites the whole file from what
// it read, so swallowing the error would replace a config that merely could not
// be read with one holding nothing but the managed keys — on every start.
func readExisting(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// writeFile replaces path with data so that the file is, at every instant,
// either the old config or the new one. Every parser here rewrites the whole
// file from what it read, on every start, from an init container that can be
// killed at any moment -- an eviction, a node under memory pressure. Written in
// place, a kill between the truncate and the last byte left the player's config
// empty or cut short. The file manager's write path was hardened against
// exactly this long ago; this one had not been.
//
// The new content goes to a temporary file beside the target, is synced, and is
// renamed over it. The file's permission bits are carried over (a config that
// holds an RCON password is often 0600 on purpose). The render runs as the
// game's user, so the replacement is owned by the user that has to read it.
//
// Two cases keep the old in-place write, because renaming would change what
// they mean: a config that is a symlink (renaming would replace the link with a
// copy and silently detach whatever it pointed at), and a writable file in a
// directory the game's user cannot create files in (it worked before, and
// atomicity is not worth breaking it for).
func writeFile(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return os.WriteFile(path, data, 0o644)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	prefix := "." + base + ".quetzal-render-"

	// A render killed between creating its temporary and renaming it leaves one
	// behind. Only one render runs per server at a time, so any found now is
	// stale.
	if stale, _ := filepath.Glob(filepath.Join(dir, globEscape(prefix)+"*")); len(stale) > 0 {
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}

	tmp, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return os.WriteFile(path, data, 0o644)
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	// Make the rename itself durable. Best-effort: not every filesystem lets a
	// directory be synced, and the replacement is already whole either way.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// globEscape quotes the characters filepath.Match treats as patterns, so a file
// name containing them matches only itself.
func globEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

// ---- properties / flat key=value ----

// applyLineKV patches a line-oriented "key<sep>value" file, preserving existing
// lines/comments/order and appending any keys not already present.
func applyLineKV(path string, vals map[string]string, sep byte, spaced bool) error {
	cur, err := readExisting(path)
	if err != nil {
		return err
	}
	vals = lineKeys(vals, func(k string) bool {
		return strings.IndexByte(k, sep) < 0 && !strings.ContainsAny(k[:1], "#!;")
	})
	lines := splitLines(string(cur))
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
	cur, err := readExisting(path)
	if err != nil {
		return err
	}
	vals = iniKeys(vals)
	lines := splitLines(string(cur))
	applied := make(map[string]bool, len(vals))
	current := "" // section name, "" = top-level

	for i, line := range lines {
		if sec, ok := iniSection(line); ok {
			current = sec
			continue
		}
		t := strings.TrimSpace(line)
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

	// The keys not found go at the end of their section, or in a new section at
	// the end of the file. A top-level key goes before the first section: after
	// it, it would belong to that section and never be found again.
	pending := map[string][]string{}
	for _, k := range sortedUnapplied(vals, applied) {
		sec, key := splitSection(k)
		pending[sec] = append(pending[sec], fmt.Sprintf("%s=%s", key, vals[k]))
	}
	out := make([]string, 0, len(lines)+len(vals)+len(pending))
	flush := func(sec string) {
		add := pending[sec]
		if len(add) == 0 {
			return
		}
		delete(pending, sec)
		// After the section's last line with content, before its blank lines.
		n := len(out)
		for n > 0 && strings.TrimSpace(out[n-1]) == "" {
			n--
		}
		tail := append([]string(nil), out[n:]...)
		out = append(append(out[:n], add...), tail...)
	}
	current = ""
	for _, line := range lines {
		if sec, ok := iniSection(line); ok {
			flush(current)
			current = sec
		}
		out = append(out, line)
	}
	flush(current)
	secs := make([]string, 0, len(pending))
	for s := range pending {
		secs = append(secs, s)
	}
	sort.Strings(secs)
	for _, s := range secs {
		out = append(out, "["+s+"]")
		out = append(out, pending[s]...)
	}
	return writeFile(path, []byte(joinLines(out)))
}

// iniSection reports whether a line is a section header, and its name.
func iniSection(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") && len(t) >= 2 {
		return strings.TrimSpace(t[1 : len(t)-1]), true
	}
	return "", false
}

// iniKeys is lineKeys for INI: "section.key" with both parts trimmed, as the
// file reads back, and without the keys no line can hold.
func iniKeys(vals map[string]string) map[string]string {
	out := make(map[string]string, len(vals))
	for _, k := range sortedKeys(vals) {
		sec, key := splitSection(k)
		sec, key = strings.TrimSpace(sec), strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(key[:1], "#;[") ||
			strings.ContainsAny(sec, "\r\n") {
			continue
		}
		full := key
		if sec != "" {
			full = sec + "." + key
		}
		// It must split back the same way: a top-level key holding a dot would
		// be read as a section.
		if s2, k2 := splitSection(full); s2 != sec || k2 != key {
			continue
		}
		out[full] = oneLine(vals[k])
	}
	return out
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
	cur, err := readExisting(path)
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if b := bytes.TrimSpace(cur); len(b) > 0 {
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
	cur, err := readExisting(path)
	if err != nil {
		return err
	}
	for k, v := range vals {
		vals[k] = oneLine(v)
	}
	// In a fixed order, so a line that starts with two keys ("query" and
	// "query.port") gets the same one on every start.
	keys := sortedKeys(vals)
	lines := splitLines(string(cur))
	applied := make(map[string]bool, len(vals))
	for i, line := range lines {
		t := strings.TrimSpace(line)
		for _, k := range keys {
			if !applied[k] && strings.HasPrefix(t, k) {
				lines[i] = vals[k]
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

// oneLine keeps a value on its line. The line-based formats write a value as
// part of a line; a line break in it would write more lines, which read back
// as keys of their own. A startup variable could then set any key of the file
// (online-mode=false, say) without access to the file, and every start would
// add those lines again.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// lineKeys prepares the keys of a flat key/value file: trimmed, as the file
// reads them back, and without the ones no line can hold (empty, holding a line
// break, or refused by ok). Values are kept on one line. Without this, a key
// the scan cannot find again is appended again on every start.
func lineKeys(vals map[string]string, ok func(key string) bool) map[string]string {
	out := make(map[string]string, len(vals))
	for _, k := range sortedKeys(vals) {
		key := strings.TrimSpace(k)
		if key == "" || strings.ContainsAny(key, "\r\n") || !ok(key) {
			continue
		}
		out[key] = oneLine(vals[k])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		// CRLF files, and any carriage return left at a line's end, which the
		// "\n" of joinLines would turn into a CRLF the next pass reads differently.
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
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
