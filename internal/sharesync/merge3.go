package sharesync

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

// The three-way merge of structured settings files (json3, toml3): a
// document is parsed into a tree of values, the two changed copies are
// merged against the content both were made from (their base), and the
// result is written back in the current copy's format.
//
// Merging a value changed on one side only takes that side's value. A value
// both sides changed alike is that value. Two objects both changed merge
// key by key, keeping the current copy's key order and adding the other's
// new keys after it. Two arrays both changed merge as sets (hook arrays,
// permission lists): an element stays when either side has it unless the
// other removed it, the current copy's order first, each element once. A
// key one side removed and the other changed keeps the change. What is
// left, two different scalars (or kinds), is a conflict: the newer change's
// value wins, the same whichever copy arrived first.

// valueKind is what a document value is.
type valueKind uint8

const (
	scalarValue valueKind = iota
	objectValue
	arrayValue
)

// value is one value of a parsed document.
type value struct {
	kind valueKind
	// keys are an object's keys in order; fields their values.
	keys   []string
	fields map[string]*value
	// rawKeys are an object's keys as the document spells them (JSON), by
	// key, so a merged file keeps its escapes.
	rawKeys map[string]string
	// elems are an array's elements.
	elems []*value
	// text is a scalar's JSON token as the document spells it.
	text string
	// native is a TOML scalar as go-toml decodes it.
	native any
	// id identifies the value: two values with the same id are the same
	// value, whatever their spelling (key order, string escapes).
	id [sha256.Size]byte
}

// newObject returns an empty object.
func newObject() *value {
	return &value{kind: objectValue, fields: map[string]*value{}, rawKeys: map[string]string{}}
}

// set sets an object's key, keeping its place when it is there already.
func (v *value) set(k, raw string, field *value) {
	if _, ok := v.fields[k]; !ok {
		v.keys = append(v.keys, k)
	}
	v.fields[k] = field
	if raw != "" {
		v.rawKeys[k] = raw
	}
}

// seal computes v's id from its scalar identity (for a scalar) or its
// members' ids, once they are sealed.
func (v *value) seal(scalar string) *value {
	h := sha256.New()
	switch v.kind {
	case scalarValue:
		h.Write([]byte{'s'})
		h.Write([]byte(scalar))
	case objectValue:
		h.Write([]byte{'o'})
		keys := slices.Clone(v.keys)
		slices.Sort(keys)
		for _, k := range keys {
			kh := sha256.Sum256([]byte(k))
			h.Write(kh[:])
			id := v.fields[k].id
			h.Write(id[:])
		}
	case arrayValue:
		h.Write([]byte{'a'})
		for _, e := range v.elems {
			h.Write(e.id[:])
		}
	}
	h.Sum(v.id[:0])
	return v
}

// same reports whether a and b are the same value, nil being no value.
func same(a, b *value) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.id == b.id
}

// isEmptyObject reports whether v is '{}'.
func isEmptyObject(v *value) bool {
	return v != nil && v.kind == objectValue && len(v.keys) == 0
}

// merger3 is one three-way merge: which side wins a conflict, and the
// conflicts found.
type merger3 struct {
	// incomingWins: the incoming change is the newer.
	incomingWins bool
	conflicts    []string
}

// merge merges the changes from base to cur and from base to inc at path
// at (nil is no value: never there, or removed).
func (m *merger3) merge(base, cur, inc *value, at string) *value {
	switch {
	case same(cur, inc):
		return cur
	case same(base, cur):
		return inc
	case same(base, inc):
		return cur
	case cur == nil:
		// Removed here, changed there: the change is kept.
		return inc
	case inc == nil:
		return cur
	case cur.kind == objectValue && inc.kind == objectValue:
		if base != nil && base.kind != objectValue {
			base = nil
		}
		return m.mergeObjects(base, cur, inc, at)
	case cur.kind == arrayValue && inc.kind == arrayValue:
		if base != nil && base.kind != arrayValue {
			base = nil
		}
		return mergeSets(base, cur, inc)
	}
	m.conflicts = append(m.conflicts, at)
	if m.incomingWins {
		return inc
	}
	return cur
}

// mergeObjects merges two changed objects key by key.
func (m *merger3) mergeObjects(base, cur, inc *value, at string) *value {
	out := newObject()
	field := func(v *value, k string) *value {
		if v == nil {
			return nil
		}
		return v.fields[k]
	}
	add := func(k string) {
		if _, done := out.fields[k]; done {
			return
		}
		merged := m.merge(field(base, k), field(cur, k), field(inc, k), joinPath(at, k))
		if merged == nil {
			return
		}
		raw := cur.rawKeys[k]
		if _, ok := cur.fields[k]; !ok {
			raw = inc.rawKeys[k]
		}
		out.set(k, raw, merged)
	}
	for _, k := range cur.keys {
		add(k)
	}
	for _, k := range inc.keys {
		add(k)
	}
	return out.seal("")
}

// mergeSets merges two changed arrays as sets: an element both copies have
// or lack stays so; one only one copy changed (added, or removed from the
// base) follows that copy. In order: the current copy's elements, then the
// incoming copy's added ones; each element once.
func mergeSets(base, cur, inc *value) *value {
	has := func(v *value) map[[sha256.Size]byte]bool {
		set := map[[sha256.Size]byte]bool{}
		if v != nil {
			for _, e := range v.elems {
				set[e.id] = true
			}
		}
		return set
	}
	inBase, inCur, inInc := has(base), has(cur), has(inc)
	keep := func(id [sha256.Size]byte) bool {
		if inCur[id] == inInc[id] {
			return inCur[id]
		}
		// One side changed it: that side's say (the one that differs from
		// the base) wins.
		return !inBase[id]
	}
	out := &value{kind: arrayValue}
	seen := map[[sha256.Size]byte]bool{}
	for _, v := range []*value{cur, inc} {
		for _, e := range v.elems {
			if seen[e.id] || !keep(e.id) {
				continue
			}
			seen[e.id] = true
			out.elems = append(out.elems, e)
		}
	}
	return out.seal("")
}

// joinPath names a key below at, for the conflict list.
func joinPath(at, k string) string {
	if at == "" {
		return k
	}
	return at + "." + k
}

// conflictNote says which values a merge decided by recency.
func conflictNote(conflicts []string) string {
	if len(conflicts) == 0 {
		return ""
	}
	const show = 5
	names := conflicts
	if len(names) > show {
		names = names[:show]
	}
	note := "; the newer change's " + strings.Join(names, ", ")
	if more := len(conflicts) - len(names); more > 0 {
		note += fmt.Sprintf(" and %d more", more)
	}
	return note + " kept"
}
