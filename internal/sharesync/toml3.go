package sharesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// TOML3 is the toml3 policy, for TOML config files (Codex's config.toml):
// json3's three-way merge over TOML tables and values (arrays merge as
// sets, a value both copies changed takes the newer change's), and a file
// that does not parse, or is empty (a write under way), waits on its side
// until a whole one is written. The merged file is the current copy with
// the other's changes written into it, so its comments, order and layout
// stay; where that cannot be done exactly (arrays of tables, a table made
// by dotted keys elsewhere), the merged config is written out plain. Two
// copies that would merge to an empty config (each removed what the other
// kept) keep the newer one, whatever comments are left. Deletes go as
// mirror.
type TOML3 struct{}

// Scope is fleet-wide.
func (TOML3) Scope() Scope { return ScopeFleet }

// Lane leaves the lane to the size.
func (TOML3) Lane() Lane { return LaneAuto }

// History keeps the bases of the three-way merge.
func (TOML3) History() int { return mergeHistory }

// Merge merges concurrent copies.
func (TOML3) Merge(ctx context.Context, in *MergeInput) (MergeResult, error) {
	return mergeDocument(ctx, in, tomlFormat{})
}

// tomlFormat is TOML for mergeDocument.
type tomlFormat struct{}

func (tomlFormat) name() string { return "TOML" }

// seed: no instance seeds a TOML file.
func (tomlFormat) seed(*value) bool { return false }

func (tomlFormat) parse(data []byte) (*value, error) {
	return parseTOML(data)
}

func (tomlFormat) render(merged *value, like []byte, cur *value) ([]byte, error) {
	if out, err := patchTOML(like, cur, merged); err == nil {
		if got, err := parseTOML(out); err == nil && same(got, merged) {
			return out, nil
		}
	}
	native, ok := tomlNative(merged).(map[string]any)
	if !ok {
		return nil, errors.New("not a table")
	}
	out, err := toml.Marshal(native)
	if err != nil {
		return nil, err
	}
	if got, err := parseTOML(out); err != nil || !same(got, merged) {
		return nil, errors.New("the merged config does not read back as merged")
	}
	return out, nil
}

// parseTOML reads a TOML document into a value. An empty file is no
// document: a config written in place reads empty for a moment.
func parseTOML(data []byte) (*value, error) {
	if len(data) == 0 {
		return nil, errors.New("an empty file")
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return tomlValue(doc), nil
}

// tomlValue converts what go-toml decodes into a value, tables' keys
// sorted.
func tomlValue(x any) *value {
	switch t := x.(type) {
	case map[string]any:
		obj := newObject()
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			obj.set(k, "", tomlValue(t[k]))
		}
		return obj.seal("")
	case []any:
		arr := &value{kind: arrayValue}
		for _, e := range t {
			arr.elems = append(arr.elems, tomlValue(e))
		}
		return arr.seal("")
	}
	return (&value{native: x}).seal(tomlScalar(x))
}

// tomlScalar is a TOML scalar's identity: its type and value (TOML is
// typed: 1 and 1.0 differ).
func tomlScalar(x any) string {
	switch t := x.(type) {
	case string:
		return "s" + t
	case int64:
		return "i" + strconv.FormatInt(t, 10)
	case float64:
		if math.IsNaN(t) {
			return "fnan"
		}
		return "f" + strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return "b" + strconv.FormatBool(t)
	case time.Time:
		return "t" + t.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("x%T:%v", x, x)
}

// tomlNative converts a value back into what go-toml encodes.
func tomlNative(v *value) any {
	switch v.kind {
	case objectValue:
		m := make(map[string]any, len(v.keys))
		for _, k := range v.keys {
			m[k] = tomlNative(v.fields[k])
		}
		return m
	case arrayValue:
		out := make([]any, 0, len(v.elems))
		for _, e := range v.elems {
			out = append(out, tomlNative(e))
		}
		return out
	}
	return v.native
}

// tomlInline writes a value as an inline TOML value.
func tomlInline(v *value) (string, error) {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).SetTablesInline(true).Encode(map[string]any{"v": tomlNative(v)}); err != nil {
		return "", err
	}
	out, ok := bytes.CutPrefix(b.Bytes(), []byte("v = "))
	if !ok {
		return "", fmt.Errorf("go-toml wrote %q", b.Bytes())
	}
	return string(bytes.TrimSuffix(out, []byte("\n"))), nil
}

// tomlKey writes a dotted key, each part bare when it can be.
func tomlKey(parts []string) string {
	var b bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('.')
		}
		if bareKey(p) {
			b.WriteString(p)
			continue
		}
		b.WriteByte('"')
		for _, r := range p {
			switch {
			case r == '"' || r == '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\t':
				b.WriteString(`\t`)
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\u%04X`, r)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	}
	return b.String()
}

// bareKey reports whether a key part can be written unquoted.
func bareKey(p string) bool {
	if p == "" {
		return false
	}
	for _, c := range []byte(p) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
