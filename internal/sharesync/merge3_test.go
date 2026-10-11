package sharesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// The merges' property tests: random documents edited twice from a common
// base, merged, and checked against the rules stated independently of
// merger3 (checkMerge), for symmetry (the same document whichever copy
// arrives first, the newer deciding conflicts), and for the round trip
// through each format's writer (JSON with each indent style, TOML through
// the patch of a commented copy).

// docGen makes random documents and edits.
type docGen struct {
	rng  *rand.Rand
	n    int
	toml bool
}

var genKeys = []string{"a", "b", "c", "model", "hooks", "x y", "é", "k.d"}

func (g *docGen) scalar() any {
	g.n++
	switch g.rng.IntN(5) {
	case 0:
		return int64(g.rng.IntN(4))
	case 1:
		return g.rng.IntN(2) == 0
	case 2:
		if g.toml {
			return float64(g.rng.IntN(3)) + 0.5
		}
		return nil
	case 3:
		return fmt.Sprintf("s%d", g.n)
	}
	return []string{"x", "y", `q"uote`, "tab\there"}[g.rng.IntN(4)]
}

func (g *docGen) val(depth int) any {
	switch k := g.rng.IntN(10); {
	case depth > 0 && k < 3:
		return g.object(depth - 1)
	case depth > 0 && k < 5:
		arr := []any{}
		for range g.rng.IntN(4) {
			if g.rng.IntN(4) == 0 && !g.toml {
				arr = append(arr, g.object(depth-1))
			} else {
				arr = append(arr, g.scalar())
			}
		}
		return arr
	}
	return g.scalar()
}

func (g *docGen) object(depth int) map[string]any {
	obj := map[string]any{}
	for range g.rng.IntN(5) {
		obj[genKeys[g.rng.IntN(len(genKeys))]] = g.val(depth)
	}
	return obj
}

// edit returns a copy of doc with a few random changes.
func (g *docGen) edit(doc map[string]any) map[string]any {
	out := deepCopy(doc).(map[string]any)
	for range 1 + g.rng.IntN(3) {
		g.mutate(out, 3)
	}
	return out
}

func (g *docGen) mutate(obj map[string]any, depth int) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) > 0 && g.rng.IntN(3) > 0 {
		k := keys[g.rng.IntN(len(keys))]
		switch v := obj[k].(type) {
		case map[string]any:
			if depth > 0 && g.rng.IntN(3) > 0 {
				g.mutate(v, depth-1)
				return
			}
		case []any:
			if g.rng.IntN(2) == 0 {
				if len(v) > 0 && g.rng.IntN(2) == 0 {
					i := g.rng.IntN(len(v))
					obj[k] = append(v[:i:i], v[i+1:]...)
				} else {
					obj[k] = append(v, g.scalar())
				}
				return
			}
		}
		if g.rng.IntN(3) == 0 {
			delete(obj, k)
			return
		}
		obj[k] = g.val(depth)
		return
	}
	obj[genKeys[g.rng.IntN(len(genKeys))]] = g.val(depth)
}

func deepCopy(x any) any {
	switch t := x.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, v := range t {
			m[k] = deepCopy(v)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = deepCopy(v)
		}
		return out
	}
	return x
}

// encode writes a generated document in the generator's format.
func (g *docGen) encode(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	if !g.toml {
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data, err := toml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// commented adds comment lines and trailing comments to a TOML document
// go-toml wrote (no multi-line strings: the generator's have no newline).
func (g *docGen) commented(data []byte) []byte {
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if g.rng.IntN(3) == 0 {
			fmt.Fprintf(&out, "# note %d\n", g.rng.IntN(100))
		}
		trimmed := strings.TrimSuffix(line, "\n")
		if strings.Contains(trimmed, " = ") && !strings.ContainsAny(trimmed, "#") && g.rng.IntN(3) == 0 {
			out.WriteString(trimmed + " # why\n")
			continue
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

func TestMergeProperties(t *testing.T) {
	seeds := 400
	if testing.Short() {
		seeds = 100
	}
	if n, err := strconv.Atoi(os.Getenv("SHARESYNC_SEEDS")); err == nil && n > 0 {
		seeds = n * 50
	}
	for _, format := range []docFormat{jsonFormat{}, tomlFormat{}} {
		for seed := range seeds {
			g := &docGen{rng: rand.New(rand.NewPCG(uint64(seed), 7)), toml: format.name() == "TOML"}
			base := g.object(3)
			a, b := g.edit(base), g.edit(base)
			if g.rng.IntN(5) == 0 {
				b = deepCopy(a).(map[string]any)
			}
			baseText, aText, bText := g.encode(t, base), g.encode(t, a), g.encode(t, b)
			if g.toml {
				aText = g.commented(aText)
			}
			name := fmt.Sprintf("%s seed %d", format.name(), seed)
			vb, err1 := format.parse(baseText)
			va, err2 := format.parse(aText)
			vbb, err3 := format.parse(bText)
			if err := errors.Join(err1, err2, err3); err != nil {
				if g.toml && len(baseText)*len(aText)*len(bText) == 0 {
					continue // an empty TOML document is no whole file
				}
				t.Fatalf("%s: %v", name, err)
			}
			checkMergeProperties(t, name, format, vb, va, vbb, aText)
		}
	}
}

// checkMergeProperties checks one merge of a (current, written as aText)
// and b (incoming) from base.
func checkMergeProperties(t *testing.T, name string, format docFormat, base, a, b *value, aText []byte) {
	t.Helper()
	for _, incomingWins := range []bool{false, true} {
		m := (&merger3{incomingWins: incomingWins}).merge(base, a, b, "")
		if why := checkMerge(base, a, b, m, incomingWins); why != "" {
			t.Fatalf("%s (incoming wins %v): %s", name, incomingWins, why)
		}
		swapped := (&merger3{incomingWins: !incomingWins}).merge(base, b, a, "")
		if !sameAsSets(m, swapped) {
			t.Fatalf("%s: the merge depends on which copy arrived first", name)
		}
		if again := (&merger3{incomingWins: incomingWins}).merge(base, m, m, ""); !same(again, m) {
			t.Fatalf("%s: merging the merge with itself changed it", name)
		}
		if one := (&merger3{}).merge(base, base, m, ""); !same(one, m) {
			t.Fatalf("%s: a one-sided change was not taken", name)
		}
		likes := [][]byte{aText}
		if format.name() == "JSON" {
			likes = append(likes, []byte("{\n  \"z\": 1\n}\n"), []byte("{\n\t\"z\": 1\n}"))
		}
		for _, like := range likes {
			out, err := format.render(m, like, a)
			if err != nil && format.name() == "TOML" && len(m.keys) == 0 {
				// A config merged to nothing cannot be written whole (an empty
				// file reads as a write under way): the policy keeps the newer
				// copy instead.
				continue
			}
			if err != nil {
				t.Fatalf("%s: render: %v", name, err)
			}
			back, err := format.parse(out)
			if err != nil || !same(back, m) {
				t.Fatalf("%s: the merged file does not read back as the merge (%v):\n%s\ncurrent:\n%s", name, err, out, like)
			}
		}
	}
}

// checkMerge states the merge's rules, independently of merger3: a value
// only one copy changed is that copy's; a value both changed alike is that
// value; two changed objects merge key by key; two changed arrays merge as
// sets; a removal loses to a change; any other conflict goes to the newer.
func checkMerge(base, a, b, m *value, bWins bool) string {
	switch {
	case same(a, b) || same(base, b):
		if !same(m, a) {
			return "a value only the current copy changed is not the current copy's"
		}
	case same(base, a):
		if !same(m, b) {
			return "a value only the incoming copy changed is not the incoming copy's"
		}
	case a == nil || b == nil:
		if want := cmpOr(a, b); !same(m, want) {
			return "a removal won over a change"
		}
	case a.kind == objectValue && b.kind == objectValue:
		if m == nil || m.kind != objectValue {
			return "two changed objects did not merge into an object"
		}
		keys := map[string]bool{}
		for _, k := range append(slices.Clone(a.keys), b.keys...) {
			keys[k] = true
		}
		for k := range m.fields {
			if !keys[k] {
				return "the merge has a key neither copy has: " + k
			}
		}
		if base != nil && base.kind != objectValue {
			base = nil
		}
		for k := range keys {
			if why := checkMerge(field(base, k), a.fields[k], b.fields[k], m.fields[k], bWins); why != "" {
				return k + ": " + why
			}
		}
	case a.kind == arrayValue && b.kind == arrayValue:
		if m == nil || m.kind != arrayValue {
			return "two changed arrays did not merge into an array"
		}
		in := func(v *value, id [sha256.Size]byte) bool {
			if v == nil || v.kind != arrayValue {
				return false
			}
			return slices.ContainsFunc(v.elems, func(e *value) bool { return e.id == id })
		}
		seen := map[[sha256.Size]byte]bool{}
		for _, e := range m.elems {
			if seen[e.id] {
				return "an element twice in a merged set"
			}
			if !in(a, e.id) && !in(b, e.id) {
				return "a merged set has an element neither copy has"
			}
			seen[e.id] = true
		}
		for _, e := range append(slices.Clone(a.elems), b.elems...) {
			inA, inB, inBase := in(a, e.id), in(b, e.id), in(base, e.id)
			want := inA && inB || inA && !inBase || inB && !inBase
			if seen[e.id] != want {
				return fmt.Sprintf("set element kept %v, want %v (in current %v, incoming %v, base %v)", seen[e.id], want, inA, inB, inBase)
			}
		}
	default:
		want := a
		if bWins {
			want = b
		}
		if !same(m, want) {
			return "a conflict did not go to the newer change"
		}
	}
	return ""
}

// sameAsSets is same with arrays compared as sets: a set merge lists the
// current copy's elements first, so two arrival orders differ in order
// only.
func sameAsSets(a, b *value) bool {
	return setID(a) == setID(b)
}

func setID(v *value) string {
	if v == nil {
		return "nil"
	}
	switch v.kind {
	case objectValue:
		keys := slices.Clone(v.keys)
		slices.Sort(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, strconv.Quote(k)+":"+setID(v.fields[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case arrayValue:
		var parts []string
		for _, e := range v.elems {
			parts = append(parts, setID(e))
		}
		slices.Sort(parts)
		return "[" + strings.Join(parts, ",") + "]"
	}
	return fmt.Sprintf("%x", v.id)
}

func field(v *value, k string) *value {
	if v == nil {
		return nil
	}
	return v.fields[k]
}

func cmpOr(a, b *value) *value {
	if a != nil {
		return a
	}
	return b
}

// FuzzJSON3Merge checks any three JSON documents merge without a panic
// into a document that reads back, never '{}' over content, the same
// whichever copy arrived first.
func FuzzJSON3Merge(f *testing.F) {
	f.Add([]byte(`{"a":1,"b":1}`), []byte(`{"a":2,"b":1}`), []byte(`{"a":1,"b":2}`))
	f.Add([]byte(`{"h":[{"m":1}]}`), []byte(`{"h":[{"m":1},{"m":2}]}`), []byte(`{"h":[]}`))
	f.Add([]byte(`{"a":1}`), []byte("{\n  \"a\": 2\n}\n"), []byte(`{}`))
	f.Add([]byte(``), []byte(`[1,2]`), []byte(`{"a":"é"}`))
	f.Fuzz(func(t *testing.T, base, cur, inc []byte) {
		fuzzMerge(t, JSON3{}, jsonFormat{}, base, cur, inc)
	})
}

// FuzzTOML3Merge is FuzzJSON3Merge for TOML, the merged file written by the
// patch (or plain), always reading back as the merge.
func FuzzTOML3Merge(f *testing.F) {
	f.Add([]byte("a = 1\nb = 1\n"), []byte("# c\na = 2 # t\nb = 1\n"), []byte("a = 1\nb = 2\n[t]\nx = 1\n"))
	f.Add([]byte(codexConfig), []byte(strings.Replace(codexConfig, "dark", "light", 1)), []byte(codexConfig+"[p.'q']\nk = [1, 2]\n"))
	f.Add([]byte("a.b = 1\n"), []byte("a.b = 2\n"), []byte("a.b = 1\na.c = \"\"\"\nx\n\"\"\"\n"))
	f.Add([]byte("[[s]]\nn = 1\n"), []byte("[[s]]\nn = 2\n"), []byte("x = 1\n[[s]]\nn = 1\n"))
	f.Fuzz(func(t *testing.T, base, cur, inc []byte) {
		fuzzMerge(t, TOML3{}, tomlFormat{}, base, cur, inc)
	})
}

func fuzzMerge(t *testing.T, policy Merger, format docFormat, base, cur, inc []byte) {
	if len(base)+len(cur)+len(inc) > 1<<14 {
		return
	}
	vc, err1 := format.parse(cur)
	vi, err2 := format.parse(inc)
	if err1 != nil || err2 != nil {
		return
	}
	for _, curNewer := range []bool{false, true} {
		c := docCase{base: string(base), cur: string(cur), inc: string(inc), curNewer: curNewer}
		res, err := policy.Merge(context.Background(), c.input())
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		var got *value
		switch res.Decision {
		case Accept:
			got = vi
		case Keep:
			got = vc
		case Merged:
			if got, err = format.parse(res.Content); err != nil {
				t.Fatalf("the merged file does not read back (%v):\n%s", err, res.Content)
			}
		default:
			t.Fatalf("decision %d", res.Decision)
		}
		if format.seed(got) && !format.seed(vc) {
			t.Fatalf("'{}' won over content (decision %d)", res.Decision)
		}
		// Swapped, with the same copy the newer: the same document.
		s := docCase{base: string(base), cur: string(inc), inc: string(cur), curNewer: !curNewer}
		sres, err := policy.Merge(context.Background(), s.input())
		if err != nil {
			t.Fatalf("swapped merge: %v", err)
		}
		var sgot *value
		switch sres.Decision {
		case Accept:
			sgot = vc
		case Keep:
			sgot = vi
		case Merged:
			sgot, _ = format.parse(sres.Content)
		}
		if res.Reason == "" || format.seed(vi) || format.seed(vc) || strings.Contains(res.Reason, "empty") || strings.Contains(sres.Reason, "empty") {
			continue // the guards are one-sided by design
		}
		if !sameAsSets(got, sgot) {
			t.Fatalf("the merge depends on which copy arrived first:\n%s\nvs\n%s", res.Content, sres.Content)
		}
	}
}

// FuzzSplitTOML checks the statement split of any TOML document covers it
// exactly, each key/value's path naming a value of the document.
func FuzzSplitTOML(f *testing.F) {
	f.Add([]byte(codexConfig))
	f.Add([]byte("a = \"\"\"\n# not a comment\n[x]\n\"\"\"\nb = '''\n'''\n"))
	f.Add([]byte("[ 'a.b' . \"c\" ] # h\n  k = { x = [1, # c\n 2] }\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := parseTOML(data)
		if err != nil {
			return
		}
		stmts, err := splitTOML(data)
		if err != nil {
			return // the patch steps aside; the plain writer takes over
		}
		end := 0
		for _, st := range stmts {
			if st.start != end || st.end <= st.start {
				t.Fatalf("statements do not tile the document at %d: %+v", end, st)
			}
			end = st.end
			if st.kind == tomlKeyValue && lookup(doc, st.path) == nil {
				t.Fatalf("key/value %q names nothing in the document", st.path)
			}
		}
		if end != len(data) {
			t.Fatalf("statements end at %d of %d", end, len(data))
		}
	})
}
