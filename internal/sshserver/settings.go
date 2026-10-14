package sshserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"sort"
)

// settings.go merges the devcontainer's settings into VS Code's machine
// settings three ways, never clobbering the user: for each setting the
// devcontainer wants, a value the file does not have is added; one that
// still holds what fleet wrote last (the base) is updated; one the user
// changed is kept, and a key fleet wrote that the user removed stays
// removed (objects merge key by key, the same way). A setting fleet wrote
// that the devcontainer no longer wants is removed if the user left it as
// fleet wrote it. The file keeps its keys' order; a file that is not plain
// JSON (VS Code allows comments) is left alone.

// mergeMachineSettings merges want into the settings file at path, base
// being what fleet merged in last. changed reports whether the file was
// rewritten.
func mergeMachineSettings(path string, base, want map[string]json.RawMessage) (changed bool, err error) {
	data, err := os.ReadFile(path)
	mode := fs.FileMode(0o644)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(want) == 0 {
			return false, nil
		}
		data = nil
	case err != nil:
		return false, err
	default:
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	cur := &object{}
	if len(bytes.TrimSpace(data)) > 0 {
		if cur, err = parseObject(data); err != nil {
			return false, fmt.Errorf("%s is not plain JSON (%v): fleet left it as it is", path, err)
		}
	}
	if !merge3(cur, objectOf(base), objectOf(want)) {
		return false, nil
	}
	out, err := cur.marshal()
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return false, err
	}
	return true, nil
}

// object is a JSON object that keeps its keys' order.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func objectOf(m map[string]json.RawMessage) *object {
	o := &object{vals: map[string]json.RawMessage{}}
	for k, v := range m {
		o.keys = append(o.keys, k)
		o.vals[k] = v
	}
	sort.Strings(o.keys)
	return o
}

func (o *object) get(k string) (json.RawMessage, bool) {
	if o == nil || o.vals == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// parseObject reads a JSON object, keeping its keys' order (a repeated key
// keeps its first place and its last value, as JSON.parse does).
func parseObject(data []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not an object")
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("an object key is not a string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text after the object")
	}
	return o, nil
}

// marshal writes the object indented with tabs, as VS Code writes it.
func (o *object) marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "\t"); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// merge3 merges want into cur against base, in place; true when cur
// changed.
func merge3(cur, base, want *object) bool {
	changed := false
	for _, k := range want.keys {
		w := want.vals[k]
		c, inCur := cur.get(k)
		b, inBase := base.get(k)
		switch {
		case !inCur:
			if inBase && equalJSON(b, w) {
				continue // the user removed what fleet wrote
			}
			cur.set(k, w)
			changed = true
		case equalJSON(c, w):
		case inBase && equalJSON(c, b):
			cur.set(k, w) // still fleet's earlier value
			changed = true
		default:
			co, cerr := parseObject(c)
			wo, werr := parseObject(w)
			if cerr != nil || werr != nil {
				continue // the user's value
			}
			var bo *object
			if inBase {
				bo, _ = parseObject(b)
			}
			if merge3(co, bo, wo) {
				data, err := co.compact()
				if err == nil {
					cur.set(k, data)
					changed = true
				}
			}
		}
	}
	if base == nil {
		return changed
	}
	for _, k := range base.keys {
		if _, wanted := want.get(k); wanted {
			continue
		}
		if c, ok := cur.get(k); ok && equalJSON(c, base.vals[k]) {
			cur.del(k)
			changed = true
		}
	}
	return changed
}

func (o *object) compact() (json.RawMessage, error) {
	out, err := o.marshal()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// equalJSON reports whether two JSON values are the same value.
func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	if decodeNumber(a, &x) != nil || decodeNumber(b, &y) != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	return reflect.DeepEqual(x, y)
}

func decodeNumber(data []byte, v *any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
