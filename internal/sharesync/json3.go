package sharesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// JSON3 is the json3 policy, for JSON settings files (Claude Code's
// settings.json, ~/.claude.json and settings.local.json): a change made on
// the current content is taken as it is; two copies changed at once merge
// three ways against the content both were made on, so what each changed
// survives (hook arrays and other lists merge as sets), and a value both
// changed takes the newer change's. The empty object '{}', which every new
// instance seeds, never replaces content, and a file that does not parse (a
// write under way, or a file saved mid-edit) waits on its side until it
// does. The merged file keeps the current copy's key order, indentation and
// escapes. Deletes go as mirror.
type JSON3 struct{}

// Scope is fleet-wide.
func (JSON3) Scope() Scope { return ScopeFleet }

// Lane leaves the lane to the size.
func (JSON3) Lane() Lane { return LaneAuto }

// History keeps the bases of the three-way merge.
func (JSON3) History() int { return mergeHistory }

// Merge merges concurrent copies.
func (JSON3) Merge(ctx context.Context, in *MergeInput) (MergeResult, error) {
	return mergeDocument(ctx, in, jsonFormat{})
}

// jsonFormat is JSON for mergeDocument.
type jsonFormat struct{}

func (jsonFormat) name() string { return "JSON" }

func (jsonFormat) seed(v *value) bool { return isEmptyObject(v) }

func (jsonFormat) parse(data []byte) (*value, error) {
	return parseJSON(data)
}

// render writes merged as like is written: indented with like's indent
// when like spans lines (JSON.stringify's two spaces, for Claude Code's
// files), compact otherwise, ending in a newline when like does.
func (jsonFormat) render(merged *value, like []byte, _ *value) ([]byte, error) {
	var compact bytes.Buffer
	writeJSON(&compact, merged)
	out := compact.Bytes()
	if indent, ok := jsonIndent(like); ok {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, out, "", indent); err != nil {
			return nil, err
		}
		out = pretty.Bytes()
	}
	if bytes.HasSuffix(like, []byte("\n")) {
		out = append(out, '\n')
	}
	return out, nil
}

// parseJSON reads a JSON document into a value, keeping each token as it is
// spelt. An object keeps a repeated key's first place and last value, as
// JSON.parse does.
func parseJSON(data []byte) (*value, error) {
	if !json.Valid(data) {
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, errors.New("an empty file")
		}
		return nil, errors.New("not valid JSON")
	}
	p := &jsonParser{data: data}
	return p.value(), nil
}

// jsonParser walks JSON already checked valid.
type jsonParser struct {
	data []byte
	pos  int
}

func (p *jsonParser) space() {
	for p.pos < len(p.data) && strings.IndexByte(" \t\r\n", p.data[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *jsonParser) value() *value {
	p.space()
	switch p.data[p.pos] {
	case '{':
		p.pos++
		obj := newObject()
		for {
			p.space()
			if p.data[p.pos] == '}' {
				p.pos++
				return obj.seal("")
			}
			if p.data[p.pos] == ',' {
				p.pos++
				p.space()
			}
			raw := p.string()
			var k string
			json.Unmarshal([]byte(raw), &k)
			p.space()
			p.pos++ // ':'
			obj.set(k, raw, p.value())
		}
	case '[':
		p.pos++
		arr := &value{kind: arrayValue}
		for {
			p.space()
			switch p.data[p.pos] {
			case ']':
				p.pos++
				return arr.seal("")
			case ',':
				p.pos++
				continue
			}
			arr.elems = append(arr.elems, p.value())
		}
	case '"':
		raw := p.string()
		var s string
		json.Unmarshal([]byte(raw), &s)
		return (&value{text: raw}).seal("s" + s)
	}
	start := p.pos
	for p.pos < len(p.data) && strings.IndexByte(" \t\r\n,]}", p.data[p.pos]) < 0 {
		p.pos++
	}
	text := string(p.data[start:p.pos])
	return (&value{text: text}).seal(jsonLiteral(text))
}

// string returns the string token at p, as spelt.
func (p *jsonParser) string() string {
	start := p.pos
	p.pos++
	for p.data[p.pos] != '"' {
		if p.data[p.pos] == '\\' {
			p.pos++
		}
		p.pos++
	}
	p.pos++
	return string(p.data[start:p.pos])
}

// jsonLiteral is a number's or literal's identity: numbers by value (1,
// 1.0 and 1e0 are one number, as in JavaScript), integers past float
// precision by their digits.
func jsonLiteral(text string) string {
	switch text {
	case "true", "false", "null":
		return "l" + text
	}
	if !strings.ContainsAny(text, ".eE") {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil && n > -(1<<53) && n < 1<<53 {
			return "n" + strconv.FormatFloat(float64(n), 'g', -1, 64)
		}
		return "n" + text
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return "n" + strconv.FormatFloat(f, 'g', -1, 64)
	}
	return "n" + text
}

// writeJSON writes v compact, each token as its document spelt it.
func writeJSON(b *bytes.Buffer, v *value) {
	switch v.kind {
	case objectValue:
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			raw := v.rawKeys[k]
			if raw == "" {
				enc, _ := json.Marshal(k)
				raw = string(enc)
			}
			b.WriteString(raw)
			b.WriteByte(':')
			writeJSON(b, v.fields[k])
		}
		b.WriteByte('}')
	case arrayValue:
		b.WriteByte('[')
		for i, e := range v.elems {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	default:
		b.WriteString(v.text)
	}
}

// jsonIndent returns the indent of a document written across lines: the
// white space opening its first member's line.
func jsonIndent(like []byte) (string, bool) {
	nl := bytes.IndexByte(like, '\n')
	if nl < 0 || nl == len(bytes.TrimRight(like, " \t\r\n")) {
		return "", false
	}
	end := nl + 1
	for end < len(like) && (like[end] == ' ' || like[end] == '\t') {
		end++
	}
	if end == nl+1 {
		return "  ", true
	}
	return string(like[nl+1 : end]), true
}
