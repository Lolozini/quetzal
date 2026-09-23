package configfile

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/beevik/etree"
)

// xmlAttrRe matches Wings' attribute form: a value written as [name='value']
// sets that attribute on the element instead of its text.
var xmlAttrRe = regexp.MustCompile(`^\[(\w+)='(.*)'\]$`)

var utf8BOM = []byte("\xef\xbb\xbf")

// applyXML sets elements of an XML file the way Wings does. A key is a dotted
// path from the root element ("dedicated.system_config.server_port" is
// <dedicated><system_config><server_port>); the element's text is replaced, or
// one of its attributes with the [name='value'] form. A path without wildcards
// is created when missing, and every element a wildcard path matches is set.
//
// Where it differs from Wings: Wings creates a missing path under whatever the
// root is, even when the key names another root, and then sets nothing — the
// file is left with empty elements that the game never asked for. Some eggs do
// name the wrong root (one Space Engineers egg reuses a key for a file whose
// root is different), and a save file that grows stray elements is not a good
// outcome, so here a key whose first part is not the root changes nothing.
func applyXML(path string, vals map[string]string) error {
	existing, err := readExisting(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	bom := bytes.HasPrefix(existing, utf8BOM)
	existing = bytes.TrimPrefix(existing, utf8BOM)

	doc := etree.NewDocument()
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := doc.ReadFromBytes(existing); err != nil {
			return fmt.Errorf("%s: not valid XML: %w", path, err)
		}
	}

	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := vals[k]
		parts := strings.Split(k, ".")
		wild := strings.Contains(k, "*")
		if doc.Root() == nil {
			if wild || parts[0] == "" {
				continue
			}
			doc.SetRoot(etree.NewElement(parts[0]))
		}
		compiled, err := etree.CompilePath("./" + strings.Join(parts, "/"))
		if err != nil {
			return fmt.Errorf("%s: key %q: %w", path, k, err)
		}
		if !wild && doc.Root().Tag == parts[0] {
			el := doc.Root()
			for _, part := range parts[1:] {
				next := el.SelectElement(part)
				if next == nil {
					next = el.CreateElement(part)
				}
				el = next
			}
		}
		for _, el := range doc.FindElementsPath(compiled) {
			if m := xmlAttrRe.FindStringSubmatch(v); m != nil {
				el.CreateAttr(m[1], m[2])
			} else {
				el.SetText(v)
			}
		}
	}

	doc.Indent(2)
	out, err := doc.WriteToBytes()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bom {
		out = append(append([]byte{}, utf8BOM...), out...)
	}
	return writeFile(path, out)
}
