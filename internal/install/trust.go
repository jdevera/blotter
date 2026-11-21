package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/tidwall/gjson"
)

// CodexHash ports discovery.rs hook_hash and handler normalization, followed by
// config/fingerprint.rs version_for_toml, at rust-v0.171.0. TOML omits None fields.
func CodexHash(matcher *string, raw string) (string, error) {
	var h struct {
		Type    string  `json:"type"`
		Command string  `json:"command"`
		Timeout *uint64 `json:"timeout"`
		Async   bool    `json:"async"`
		Status  *string `json:"statusMessage"`
		Limit   *uint64 `json:"additionalContextLimit"`
	}
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return "", err
	}
	if h.Type != "command" || strings.TrimSpace(h.Command) == "" {
		return "", fmt.Errorf("invalid Codex command handler")
	}
	timeout := uint64(600)
	if h.Timeout != nil {
		timeout = max(1, *h.Timeout)
	}
	handler := map[string]any{"type": "command", "command": h.Command, "timeout": timeout, "async": h.Async}
	if h.Status != nil {
		handler["statusMessage"] = *h.Status
	}
	if h.Limit != nil && *h.Limit != 2500 {
		handler["additionalContextLimit"] = *h.Limit
	}
	identity := map[string]any{"event_name": "pre_tool_use", "hooks": []any{handler}}
	if matcher != nil {
		identity["matcher"] = *matcher
	}
	var b bytes.Buffer
	canonical(&b, identity)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b.Bytes())), nil
}
func canonical(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			jsonString(b, k)
			b.WriteByte(':')
			canonical(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canonical(b, item)
		}
		b.WriteByte(']')
	case string:
		jsonString(b, x)
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	default:
		panic("unsupported trust identity type")
	}
}
func jsonString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
func trustKey(path string, h handler) string {
	return fmt.Sprintf("%s:pre_tool_use:%d:%d", path, h.Group, h.Index)
}

type tableSpan struct {
	Key        []string
	Start, End int
	Values     map[string][2]int
}

func tomlTables(raw []byte) ([]tableSpan, error) {
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	p := unstable.Parser{}
	p.Reset(raw)
	tables := []tableSpan{}
	current := -1
	for p.NextExpression() {
		n := p.Expression()
		if n.Kind == unstable.Table || n.Kind == unstable.ArrayTable {
			it := n.Key()
			keys := []string{}
			offset := 0
			for it.Next() {
				k := it.Node()
				if len(keys) == 0 {
					offset = int(k.Raw.Offset)
				}
				keys = append(keys, string(k.Data))
			}
			start := bytes.LastIndexByte(raw[:offset], '\n') + 1
			if current >= 0 {
				tables[current].End = start
			}
			tables = append(tables, tableSpan{keys, start, len(raw), map[string][2]int{}})
			current = len(tables) - 1
		} else if n.Kind == unstable.KeyValue && current >= 0 {
			it := n.Key()
			keys := []string{}
			for it.Next() {
				keys = append(keys, string(it.Node().Data))
			}
			if len(keys) == 1 {
				v := n.Value()
				tables[current].Values[keys[0]] = [2]int{int(v.Raw.Offset), int(v.Raw.Offset + v.Raw.Length)}
			}
		}
	}
	return tables, p.Error()
}
func findTrust(raw []byte, key string) (tableSpan, bool, error) {
	tables, err := tomlTables(raw)
	if err != nil {
		return tableSpan{}, false, err
	}
	for _, t := range tables {
		if len(t.Key) == 3 && t.Key[0] == "hooks" && t.Key[1] == "state" && t.Key[2] == key {
			return t, true, nil
		}
	}
	return tableSpan{}, false, nil
}
func trustState(raw []byte, key string) (hash string, disabled bool, err error) {
	var doc struct {
		Hooks struct {
			State map[string]struct {
				Hash    string `toml:"trusted_hash"`
				Enabled *bool  `toml:"enabled"`
			} `toml:"state"`
		} `toml:"hooks"`
	}
	if err = toml.Unmarshal(raw, &doc); err != nil {
		return
	}
	s := doc.Hooks.State[key]
	return s.Hash, s.Enabled != nil && !*s.Enabled, nil
}
func patchTrust(raw []byte, key, hash string) ([]byte, error) {
	t, ok, err := findTrust(raw, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("trust state %q needs an explicit [hooks.state.\"key\"] table", key)
	}
	r, exists := t.Values["trusted_hash"]
	if !exists {
		return nil, fmt.Errorf("trust state %q has no editable trusted_hash field", key)
	}
	value := strconv.Quote(hash)
	out := append([]byte{}, raw[:r[0]]...)
	out = append(out, value...)
	out = append(out, raw[r[1]:]...)
	return out, nil
}
func restoreTrust(raw []byte, key string, prior *string) ([]byte, error) {
	t, ok, err := findTrust(raw, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return raw, nil
	}
	replacement := ""
	if prior != nil {
		replacement = *prior
	}
	out := append([]byte{}, raw[:t.Start]...)
	out = append(out, replacement...)
	out = append(out, raw[t.End:]...)
	return out, nil
}
func detectInline(raw []byte) error {
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if hooks, ok := doc["hooks"].(map[string]any); ok {
		for key, value := range hooks {
			if key != "state" {
				if groups, ok := value.([]any); ok && len(groups) > 0 {
					return fmt.Errorf("inline Codex hook handlers are unsupported in v1; move hooks.%s to hooks.json", key)
				}
			}
		}
	}
	return nil
}
func matcherPointer(group gjson.Result) *string {
	v := group.Get("matcher")
	if v.Type == gjson.String {
		s := v.String()
		return &s
	}
	return nil
}
