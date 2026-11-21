package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

type member struct {
	key   string
	value any
}

// objectLayout is the whitespace an object already uses, so members blotter
// adds read like the ones a person wrote.
type objectLayout struct {
	multiline     bool
	indent, close string // member and closing-brace indentation (multiline)
	colon         string
}

func layoutOf(raw string) objectLayout {
	l := objectLayout{colon: ":"}
	body := strings.TrimSpace(raw)
	if len(body) < 2 {
		return l
	}
	inner := body[1 : len(body)-1]
	lead := inner[:len(inner)-len(strings.TrimLeft(inner, " \t\r\n"))]
	if i := strings.LastIndexByte(lead, '\n'); i >= 0 {
		l.multiline = true
		l.indent = lead[i+1:]
		trail := inner[len(strings.TrimRight(inner, " \t\r\n")):]
		if j := strings.LastIndexByte(trail, '\n'); j >= 0 {
			l.close = trail[j+1:]
		}
	}
	// Spacing after the first key's colon decides the separator style.
	if first := gjson.Parse(inner[len(lead):]); first.Type == gjson.String {
		rest := strings.TrimLeft(inner[len(lead)+len(first.Raw):], " \t\r\n")
		if strings.HasPrefix(rest, ": ") || strings.HasPrefix(rest, ":\t") {
			l.colon = ": "
		}
	}
	return l
}

func (l objectLayout) encode(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if l.multiline {
		unit := strings.TrimPrefix(l.indent, l.close)
		if unit == "" || unit == l.indent {
			unit = "  "
		}
		enc.SetIndent(l.indent, unit)
	}
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func (l objectLayout) object(members []string) string {
	if len(members) == 0 {
		return "{}"
	}
	if !l.multiline {
		sep := ","
		if l.colon == ": " {
			sep = ", "
		}
		return "{" + strings.Join(members, sep) + "}"
	}
	return "{\n" + l.indent + strings.Join(members, ",\n"+l.indent) + "\n" + l.close + "}"
}

// editObject sets and removes members of a JSON object. Untouched members keep
// their raw text; replaced members keep their position; new members are
// appended in order, in the object's own layout and without HTML escaping.
func editObject(raw string, set []member, remove ...string) (string, error) {
	obj := gjson.Parse(raw)
	if !obj.IsObject() {
		return "", fmt.Errorf("handler must be a JSON object")
	}
	l := layoutOf(raw)
	encoded := map[string]string{}
	for _, m := range set {
		text, err := l.encode(m.value)
		if err != nil {
			return "", err
		}
		encoded[m.key] = text
	}
	drop := map[string]bool{}
	for _, k := range remove {
		drop[k] = true
	}
	seen := map[string]bool{}
	members := []string{}
	obj.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		switch text, ok := encoded[key]; {
		case drop[key]:
		case ok:
			seen[key] = true
			members = append(members, k.Raw+l.colon+text)
		default:
			members = append(members, k.Raw+l.colon+v.Raw)
		}
		return true
	})
	for _, m := range set {
		if !seen[m.key] {
			key, err := l.encode(m.key)
			if err != nil {
				return "", err
			}
			members = append(members, key+l.colon+encoded[m.key])
		}
	}
	return l.object(members), nil
}
