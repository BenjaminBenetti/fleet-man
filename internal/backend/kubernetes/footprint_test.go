package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useFootprintAt points the footprint at a temp file for the test.
func useFootprintAt(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kube", "footprint.json")
	old := FootprintPath
	FootprintPath = func() string { return path }
	t.Cleanup(func() { FootprintPath = old })
	return path
}

func TestTombstones(t *testing.T) {
	path := useFootprintAt(t)
	if f, err := LoadFootprint(); err != nil || len(f.Tombstones) != 0 {
		t.Fatalf("no file is an empty footprint: %+v %v", f, err)
	}
	at := time.Unix(1700000000, 0).UTC()
	if err := AddTombstone(Tombstone{Cluster: "c1", Key: "aaaaaaaa", Fleet: "alpha", Instance: "one", DestroyedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := AddTombstone(Tombstone{Cluster: "c2", Key: "bbbbbbbb"}); err != nil {
		t.Fatal(err)
	}
	// The same key again replaces its tombstone.
	if err := AddTombstone(Tombstone{Cluster: "c1", Key: "aaaaaaaa", DestroyedAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("footprint file %v %v", fi, err)
	}
	f, err := LoadFootprint()
	if err != nil {
		t.Fatal(err)
	}
	if c1 := f.TombstonesOf("c1"); len(c1) != 1 || !c1[0].DestroyedAt.Equal(at.Add(time.Hour)) || len(f.Tombstones) != 2 {
		t.Fatalf("tombstones %+v", f.Tombstones)
	}
	if b := f.TombstonesOf("c2"); len(b) != 1 || b[0].DestroyedAt.IsZero() {
		t.Fatalf("a tombstone gets its time: %+v", b)
	}

	before, _ := os.ReadFile(path)
	mtime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, mtime, mtime)
	if err := DropTombstone("c1", "cccccccc"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(mtime) {
		t.Error("dropping a tombstone that is not there rewrote the file")
	}
	if err := DropTombstone("c1", "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if f, _ := LoadFootprint(); len(f.TombstonesOf("c1")) != 0 || len(f.TombstonesOf("c2")) != 1 || string(before) == string(after) {
		t.Fatalf("after the drop: %+v", f.Tombstones)
	}
}

// Fields a newer fleet recorded survive this one's writes; a newer version
// is never written.
func TestFootprintKeepsWhatItDoesNotKnow(t *testing.T) {
	path := useFootprintAt(t)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(`{"v":1,"tombstones":[],"shareUIDs":{"c1/alpha":1000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddTombstone(Tombstone{Cluster: "c1", Key: "aaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var m struct {
		ShareUIDs map[string]int `json:"shareUIDs"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.ShareUIDs["c1/alpha"] != 1000 {
		t.Fatalf("unknown field lost: %s", data)
	}

	if err := os.WriteFile(path, []byte(`{"v":2,"tombstones":[{"cluster":"c1","key":"aaaaaaaa"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddTombstone(Tombstone{Cluster: "c1", Key: "bbbbbbbb"}); !errors.Is(err, ErrFootprintNewer) {
		t.Fatalf("wrote a newer footprint: %v", err)
	}
	f, err := LoadFootprint()
	if !errors.Is(err, ErrFootprintNewer) || len(f.TombstonesOf("c1")) != 1 {
		t.Fatalf("a newer footprint still reads: %+v %v", f, err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"v":2`) {
		t.Fatalf("newer file changed: %s", data)
	}
}

func TestFootprintCapsTombstones(t *testing.T) {
	f := &Footprint{V: FootprintVersion}
	base := time.Unix(1700000000, 0)
	for i := 0; i < maxTombstones+5; i++ {
		f.AddTombstone(Tombstone{Cluster: "c", Key: fmt.Sprintf("k%07d", i), DestroyedAt: base.Add(time.Duration(i) * time.Second)})
	}
	if len(f.Tombstones) != maxTombstones {
		t.Fatalf("%d tombstones kept", len(f.Tombstones))
	}
	for _, ts := range f.Tombstones {
		if ts.DestroyedAt.Before(base.Add(5 * time.Second)) {
			t.Fatalf("an oldest tombstone survived the cap: %v", ts.DestroyedAt)
		}
	}
}
