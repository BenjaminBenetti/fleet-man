package sharesync

import (
	"bytes"
	"errors"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

// toml_patch.go writes a merged TOML config as the current copy with the
// merge's changes written into it: a key/value whose value changed gets the
// new value in place (its key, spacing and trailing comment kept), one the
// merge removed goes, and a new one is written after the last key/value of
// its table (or in a new table at the end). Everything else (comments,
// blank lines, order, quoting) is the current copy's. The caller checks the
// result reads back as the merge and writes the config out plain when it
// does not, or when the layout is beyond this (arrays of tables).

// tomlStmt is one top-level statement of a TOML document: its bytes (its
// line, or lines for a multi-line value, newline included) and what it is.
type tomlStmt struct {
	start, end int
	kind       tomlKind
	// path is a header's table, or a key/value's full key: its table's
	// path, then its dotted key.
	path []string
	// table is the table a key/value sits in (the last header above it).
	table []string
	// eq is the offset of a key/value's '=', valueEnd that of the end of
	// its value (before white space and a trailing comment).
	eq, valueEnd int
}

// tomlKind is what a statement is.
type tomlKind uint8

const (
	// tomlTrivia is a blank or comment line.
	tomlTrivia tomlKind = iota
	tomlHeader
	tomlArrayHeader
	tomlKeyValue
)

// errTOMLLayout: the document's layout is beyond the patch.
var errTOMLLayout = errors.New("the config's layout cannot be patched")

// splitTOML splits a document into its statements.
func splitTOML(data []byte) ([]tomlStmt, error) {
	var out []tomlStmt
	var table []string
	for pos := 0; pos < len(data); {
		i := pos
		for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
			i++
		}
		st := tomlStmt{start: pos}
		switch {
		case i == len(data) || data[i] == '\n' || data[i] == '\r' || data[i] == '#':
			st.kind, st.end = tomlTrivia, lineEnd(data, i)
		case data[i] == '[':
			end, err := scanHeader(data, i)
			if err != nil {
				return nil, err
			}
			path, kind, err := headerPath(data[i:end])
			if err != nil {
				return nil, err
			}
			st.kind, st.end, st.path = kind, end, path
			table = path
		default:
			end, eq, valueEnd, err := scanKeyValue(data, i)
			if err != nil {
				return nil, err
			}
			key, err := keyPath(data[i:eq])
			if err != nil {
				return nil, err
			}
			st.kind, st.end, st.eq, st.valueEnd = tomlKeyValue, end, eq, valueEnd
			st.table, st.path = table, append(slices.Clone(table), key...)
		}
		out = append(out, st)
		pos = st.end
	}
	return out, nil
}

// lineEnd returns the offset after the newline ending the line at i.
func lineEnd(data []byte, i int) int {
	if j := bytes.IndexByte(data[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(data)
}

// scanHeader returns the end of the header line at i.
func scanHeader(data []byte, i int) (int, error) {
	array := i+1 < len(data) && data[i+1] == '['
	for j := i + 1; j < len(data); {
		switch data[j] {
		case '"', '\'':
			k, err := skipString(data, j)
			if err != nil {
				return 0, err
			}
			j = k
			continue
		case '\n':
			return 0, errTOMLLayout
		case ']':
			if array && j == i+1 {
				j++
				continue
			}
			return lineEnd(data, j), nil
		}
		j++
	}
	return 0, errTOMLLayout
}

// scanKeyValue scans the key/value at i: the end of its last line, its
// '=' and the end of its value.
func scanKeyValue(data []byte, i int) (end, eq, valueEnd int, err error) {
	j := i
	for ; j < len(data) && data[j] != '='; j++ {
		switch data[j] {
		case '"', '\'':
			k, err := skipString(data, j)
			if err != nil {
				return 0, 0, 0, err
			}
			j = k - 1
		case '\n', '#':
			return 0, 0, 0, errTOMLLayout
		}
	}
	if j == len(data) {
		return 0, 0, 0, errTOMLLayout
	}
	eq, valueEnd = j, -1
	depth := 0
	for j = eq + 1; j < len(data); j++ {
		switch c := data[j]; c {
		case '"', '\'':
			k, err := skipString(data, j)
			if err != nil {
				return 0, 0, 0, err
			}
			j = k - 1
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case '#':
			if depth == 0 && valueEnd < 0 {
				valueEnd = trimEnd(data, eq+1, j)
			}
			for j+1 < len(data) && data[j+1] != '\n' {
				j++
			}
		case '\n':
			if depth <= 0 {
				if valueEnd < 0 {
					valueEnd = trimEnd(data, eq+1, j)
				}
				return j + 1, eq, valueEnd, nil
			}
		}
	}
	if valueEnd < 0 {
		valueEnd = trimEnd(data, eq+1, len(data))
	}
	return len(data), eq, valueEnd, nil
}

// trimEnd returns end moved back over white space, not before start.
func trimEnd(data []byte, start, end int) int {
	for end > start && strings.IndexByte(" \t\r\n", data[end-1]) >= 0 {
		end--
	}
	return end
}

// skipString returns the offset after the string opening at j.
func skipString(data []byte, j int) (int, error) {
	q := data[j]
	if j+2 < len(data) && data[j+1] == q && data[j+2] == q {
		for k := j + 3; k < len(data); k++ {
			if q == '"' && data[k] == '\\' {
				k++
				continue
			}
			if k+2 < len(data) && data[k] == q && data[k+1] == q && data[k+2] == q {
				// Up to two quotes more belong to the string.
				end := k + 3
				for end < len(data) && data[end] == q && end-k < 5 {
					end++
				}
				return end, nil
			}
		}
		return 0, errTOMLLayout
	}
	for k := j + 1; k < len(data); k++ {
		switch data[k] {
		case '\\':
			if q == '"' {
				k++
			}
		case q:
			return k + 1, nil
		case '\n':
			return 0, errTOMLLayout
		}
	}
	return 0, errTOMLLayout
}

// headerPath parses a table header line.
func headerPath(line []byte) ([]string, tomlKind, error) {
	var p unstable.Parser
	p.Reset(line)
	if !p.NextExpression() {
		return nil, 0, errTOMLLayout
	}
	e := p.Expression()
	kind := tomlHeader
	switch e.Kind {
	case unstable.Table:
	case unstable.ArrayTable:
		kind = tomlArrayHeader
	default:
		return nil, 0, errTOMLLayout
	}
	return keyParts(e.Key()), kind, nil
}

// keyPath parses a key/value's (dotted) key.
func keyPath(key []byte) ([]string, error) {
	var p unstable.Parser
	p.Reset(append(slices.Clone(key), "= 0"...))
	if !p.NextExpression() || p.Expression().Kind != unstable.KeyValue {
		return nil, errTOMLLayout
	}
	return keyParts(p.Expression().Key()), nil
}

func keyParts(it unstable.Iterator) []string {
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// lookup returns the value at path in v, or nil.
func lookup(v *value, path []string) *value {
	for _, k := range path {
		if v == nil || v.kind != objectValue {
			return nil
		}
		v = v.fields[k]
	}
	return v
}

// pathKey makes a key path a map key.
func pathKey(path []string) string {
	return strings.Join(path, "\x00")
}

// patchTOML writes merged as data (whose parse is cur) with the merge's
// changes written in.
func patchTOML(data []byte, cur, merged *value) ([]byte, error) {
	stmts, err := splitTOML(data)
	if err != nil {
		return nil, err
	}
	n := len(stmts)
	drop := make([]bool, n)
	repl := make([]string, n)
	covered := map[string]bool{}
	headers := map[string]int{}
	firstHeader := n
	for i, st := range stmts {
		switch st.kind {
		case tomlArrayHeader:
			return nil, errTOMLLayout
		case tomlHeader:
			firstHeader = min(firstHeader, i)
			headers[pathKey(st.path)] = i
			if v := lookup(merged, st.path); v == nil || v.kind != objectValue {
				drop[i] = true
			}
		case tomlKeyValue:
			covered[pathKey(st.path)] = true
			mv := lookup(merged, st.path)
			switch {
			case mv == nil:
				drop[i] = true
			case !same(mv, lookup(cur, st.path)):
				text, err := tomlInline(mv)
				if err != nil {
					return nil, err
				}
				repl[i] = string(data[st.start:st.eq+1]) + " " + text + string(data[st.valueEnd:st.end])
			}
		}
	}
	// A table removed takes its comments with it: those between its header
	// and its last key/value (the ones after may head the next table).
	for i, st := range stmts {
		if st.kind != tomlHeader || !drop[i] {
			continue
		}
		last := i
		for j := i + 1; j < n && stmts[j].kind != tomlHeader && stmts[j].kind != tomlArrayHeader; j++ {
			if stmts[j].kind == tomlKeyValue {
				last = j
			}
		}
		for j := i + 1; j < last; j++ {
			if stmts[j].kind == tomlTrivia {
				drop[j] = true
			}
		}
	}
	isCovered := func(path []string) bool {
		for i := range path {
			if covered[pathKey(path[:i+1])] {
				return true
			}
		}
		return false
	}
	// lastKV returns the last key/value kept from from on, before the next
	// header, or from.
	lastKV := func(from int) int {
		last := from
		for j := from + 1; j < n && (stmts[j].kind == tomlKeyValue || stmts[j].kind == tomlTrivia); j++ {
			if stmts[j].kind == tomlKeyValue && !drop[j] {
				last = j
			}
		}
		return last
	}
	// rootAnchor: after the root table's last key/value, or before the
	// first header and the comment lines above it.
	rootAnchor := func() int {
		for j := firstHeader - 1; j >= 0; j-- {
			if stmts[j].kind == tomlKeyValue && !drop[j] {
				return j
			}
		}
		c := firstHeader
		for c > 0 && stmts[c-1].kind == tomlTrivia && bytes.HasPrefix(bytes.TrimLeft(data[stmts[c-1].start:stmts[c-1].end], " \t"), []byte("#")) {
			c--
		}
		return c - 1
	}
	// dottedHome finds a kept key/value defining keys of table by a dotted
	// key, for a table no header makes.
	dottedHome := func(table []string) int {
		for j := n - 1; j >= 0; j-- {
			st := stmts[j]
			if st.kind == tomlKeyValue && !drop[j] && len(st.table) < len(table) && len(st.path) > len(table) &&
				slices.Equal(st.path[:len(table)], table) {
				return j
			}
		}
		return -1
	}
	indentOf := func(i int) string {
		if i < 0 || stmts[i].kind != tomlKeyValue {
			return ""
		}
		line := data[stmts[i].start:stmts[i].end]
		return string(line[:len(line)-len(bytes.TrimLeft(line, " \t"))])
	}
	adds := map[int][]string{}
	var blocks []string
	var walk func(table []string, obj *value) error
	walk = func(table []string, obj *value) error {
		type line struct {
			key  []string
			text string
		}
		var lines []line
		for _, k := range obj.keys {
			p := append(slices.Clone(table), k)
			if v := obj.fields[k]; v.kind != objectValue && !isCovered(p) {
				text, err := tomlInline(v)
				if err != nil {
					return err
				}
				lines = append(lines, line{key: []string{k}, text: text})
			}
		}
		_, hasHeader := headers[pathKey(table)]
		switch {
		case len(lines) == 0:
			if len(obj.keys) == 0 && len(table) > 0 && !hasHeader {
				blocks = append(blocks, "["+tomlKey(table)+"]\n")
			}
		case len(table) == 0 || hasHeader:
			at := rootAnchor()
			if len(table) > 0 {
				at = lastKV(headers[pathKey(table)])
			}
			for _, l := range lines {
				adds[at] = append(adds[at], indentOf(at)+tomlKey(l.key)+" = "+l.text+"\n")
			}
		default:
			if at := dottedHome(table); at >= 0 {
				rel := table[len(stmts[at].table):]
				for _, l := range lines {
					adds[at] = append(adds[at], indentOf(at)+tomlKey(append(slices.Clone(rel), l.key...))+" = "+l.text+"\n")
				}
				break
			}
			var b strings.Builder
			b.WriteString("[" + tomlKey(table) + "]\n")
			for _, l := range lines {
				b.WriteString(tomlKey(l.key) + " = " + l.text + "\n")
			}
			blocks = append(blocks, b.String())
		}
		for _, k := range obj.keys {
			p := append(slices.Clone(table), k)
			if v := obj.fields[k]; v.kind == objectValue && !isCovered(p) {
				if err := walk(p, v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(nil, merged); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	newline := func() {
		if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
			b.WriteByte('\n')
		}
	}
	emit := func(lines []string) {
		if len(lines) == 0 {
			return
		}
		newline()
		for _, l := range lines {
			b.WriteString(l)
		}
	}
	emit(adds[-1])
	for i, st := range stmts {
		switch {
		case drop[i]:
		case repl[i] != "":
			b.WriteString(repl[i])
		default:
			b.Write(data[st.start:st.end])
		}
		emit(adds[i])
	}
	for _, blk := range blocks {
		newline()
		if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n\n")) {
			b.WriteByte('\n')
		}
		b.WriteString(blk)
	}
	return b.Bytes(), nil
}
