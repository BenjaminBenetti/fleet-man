package podspec

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden files under testdata are rewritten instead of checked with
// FLEET_PODSPEC_UPDATE=1; review the diff before committing it.
func updating() bool { return os.Getenv("FLEET_PODSPEC_UPDATE") == "1" }

// checkGolden compares got with the golden file at path (under testdata),
// or rewrites it when updating.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	path = filepath.Join("testdata", path)
	if updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (record it with FLEET_PODSPEC_UPDATE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from what Translate gives now; if the change is intended, record it with FLEET_PODSPEC_UPDATE=1 and review the diff\n%s", path, lineDiff(string(want), string(got)))
	}
}

// lineDiff shows the lines of want and got that differ, in a short form.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		b.WriteString("line " + strconv.Itoa(i+1) + ":\n  - " + wl + "\n  + " + gl + "\n")
		if shown++; shown == 20 {
			b.WriteString("  ...\n")
			break
		}
	}
	return b.String()
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// mustIndent is v as indented JSON with a trailing newline.
func mustIndent(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeFS is a daemon-host filesystem for bind classification: each path has
// a mode, and a directory's entries are the paths directly below it.
type fakeFS map[string]fs.FileMode

func (f fakeFS) Stat(name string) (fs.FileInfo, error) {
	mode, ok := f[filepath.Clean(name)]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: filepath.Base(name), mode: mode}, nil
}

// EvalSymlinks: a fakeFS has no links.
func (f fakeFS) EvalSymlinks(name string) (string, error) {
	if _, err := f.Stat(name); err != nil {
		return "", err
	}
	return filepath.Clean(name), nil
}

func (f fakeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = filepath.Clean(name)
	if mode, ok := f[name]; !ok || !mode.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	var out []fs.DirEntry
	for p, mode := range f {
		if filepath.Dir(p) == name && p != name {
			out = append(out, fs.FileInfoToDirEntry(fakeInfo{name: filepath.Base(p), mode: mode}))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// flatten writes v (decoded JSON) as a map from JSON pointer to the JSON of
// each leaf, so two pods can be compared field by field.
func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "{}"
		}
		for k, e := range x {
			flatten(prefix+"/"+k, e, out)
		}
	case []any:
		if len(x) == 0 {
			out[prefix] = "[]"
		}
		for i, e := range x {
			flatten(prefix+"/"+strconv.Itoa(i), e, out)
		}
	default:
		out[prefix] = jsonString(x)
	}
}

// changes is what differs between two pods, by JSON pointer: the new value,
// or null for a field that went away.
func changes(t *testing.T, base, got any) map[string]json.RawMessage {
	t.Helper()
	decode := func(v any) map[string]string {
		var tree any
		if err := json.Unmarshal(mustIndent(t, v), &tree); err != nil {
			t.Fatal(err)
		}
		flat := map[string]string{}
		flatten("", tree, flat)
		return flat
	}
	b, g := decode(base), decode(got)
	out := map[string]json.RawMessage{}
	for k, v := range g {
		if b[k] != v {
			out[k] = json.RawMessage(v)
		}
	}
	for k := range b {
		if _, ok := g[k]; ok {
			continue
		}
		// An empty object or list that gained members did not go away.
		filled := false
		for gk := range g {
			if strings.HasPrefix(gk, k+"/") {
				filled = true
				break
			}
		}
		if !filled {
			out[k] = json.RawMessage("null")
		}
	}
	return out
}
