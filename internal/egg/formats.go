package egg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// orderedStrings is a JSON object of strings decoded in document order. The
// order carries meaning in an egg: the first docker image is the default one
// (what Pterodactyl preselects for a new server), and the first startup command
// is Pelican's default. A Go map would pick one at random.
type orderedStrings []keyValue

type keyValue struct{ Key, Value string }

func (o *orderedStrings) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*o = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("expected an object")
	}
	var out orderedStrings
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		var v flexString
		if err := dec.Decode(&v); err != nil {
			return err
		}
		out = append(out, keyValue{Key: kt.(string), Value: string(v)})
	}
	*o = out
	return nil
}

// flexString accepts a JSON string, number or boolean (null is empty). Egg
// exports write default values as strings, but a YAML egg — or a hand-edited
// one — writes `default_value: 25565` as the number it looks like.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*f = ""
	case bool:
		*f = flexString(strconv.FormatBool(x))
	case float64:
		*f = flexString(strconv.FormatFloat(x, 'f', -1, 64))
	default:
		return fmt.Errorf("expected a string, got %s", b)
	}
	return nil
}

// eggRules is a variable's validation rules: a Laravel rule string
// ("required|string|max:20", Pterodactyl) or a list of rules (Pelican).
type eggRules string

func (r *eggRules) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*r = eggRules(s)
		return nil
	}
	var list []flexString
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("rules must be a string or a list")
	}
	parts := make([]string, 0, len(list))
	for _, p := range list {
		parts = append(parts, string(p))
	}
	*r = eggRules(strings.Join(parts, "|"))
	return nil
}

// yamlToJSON converts a YAML document (Pelican's egg format since PLCN_v3)
// into JSON, keeping mapping keys in document order for orderedStrings.
func yamlToJSON(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty document")
	}
	var buf bytes.Buffer
	if err := writeNodeJSON(&buf, doc.Content[0], 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeNodeJSON(buf *bytes.Buffer, n *yaml.Node, depth int) error {
	if depth > 64 {
		return errors.New("document nested too deeply")
	}
	switch n.Kind {
	case yaml.AliasNode:
		return errors.New("YAML aliases are not supported in an egg")
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteByte(',')
			}
			k, _ := json.Marshal(n.Content[i].Value)
			buf.Write(k)
			buf.WriteByte(':')
			if err := writeNodeJSON(buf, n.Content[i+1], depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeNodeJSON(buf, c, depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			buf.WriteString("null")
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return err
			}
			b, err := json.Marshal(v)
			if err != nil {
				// NaN and infinities have no JSON form; keep the text.
				b, _ = json.Marshal(n.Value)
			}
			buf.Write(b)
		default:
			b, _ := json.Marshal(n.Value)
			buf.Write(b)
		}
	default:
		return fmt.Errorf("unsupported YAML node")
	}
	return nil
}

// isJSON reports whether a document starts like JSON rather than YAML.
func isJSON(data []byte) bool {
	t := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

// emptyObject reports whether b is `{}`: PHP serializes an empty array as an
// empty mapping, so Pelican's YAML eggs write `file_denylist: {  }` for "none".
func emptyObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && len(m) == 0
}

// stringList is a list of strings that also accepts null and an empty object.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	if emptyObject(b) {
		*l = nil
		return nil
	}
	var s []string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*l = s
	return nil
}

// eggVariables is the variable list, also accepting an empty object.
type eggVariables []eggVariable

func (v *eggVariables) UnmarshalJSON(b []byte) error {
	if emptyObject(b) {
		*v = nil
		return nil
	}
	var list []eggVariable
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	*v = list
	return nil
}
