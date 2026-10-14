package sharesync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTestJournal opens a journal and decodes its records as strings.
func openTestJournal(t *testing.T, path string) (*journal, map[string]string) {
	t.Helper()
	j, raw, err := openJournal(path)
	if err != nil {
		t.Fatalf("openJournal: %v", err)
	}
	out := map[string]string{}
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("record %q: %v", k, err)
		}
		out[k] = s
	}
	return j, out
}

// TestJournalReplaysItsLog checks records written survive a reopen, through
// the log alone and through a compaction, deletes included, and that a
// compaction leaves an empty log.
func TestJournalReplaysItsLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "root.json")
	j, got := openTestJournal(t, path)
	if len(got) != 0 {
		t.Fatalf("a new journal holds %v", got)
	}
	j.put("a", "1")
	j.put("b", "2")
	if err := j.commit(); err != nil {
		t.Fatal(err)
	}
	j.put("a", "3")
	j.del("b")
	j.put(metaKey, "meta")
	if err := j.commit(); err != nil {
		t.Fatal(err)
	}
	j.close()

	j, got = openTestJournal(t, path)
	if got["a"] != "3" || got[metaKey] != "meta" || len(got) != 2 {
		t.Fatalf("after replay: %v", got)
	}
	if err := j.compact(map[string]any{"a": "3", "c": "4", metaKey: "meta"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".log"); err != nil || info.Size() != 0 {
		t.Errorf("log after compaction: %v, %v", info, err)
	}
	j.put("d", "5")
	j.commit()
	j.close()
	_, got = openTestJournal(t, path)
	if got["a"] != "3" || got["c"] != "4" || got["d"] != "5" || len(got) != 4 {
		t.Errorf("after compaction and more: %v", got)
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o700 {
		t.Errorf("state folder mode %v, want 0700", info.Mode())
	}
}

// TestJournalCutsOffAPartlyWrittenBatch checks a batch the disk took only part
// of (it filled up) is cut off the log again: the records written after it
// would otherwise sit behind a torn line that replay stops at, and be lost
// on the next open.
func TestJournalCutsOffAPartlyWrittenBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.json")
	j, _ := openTestJournal(t, path)
	j.put("a", "1")
	if err := j.commit(); err != nil {
		t.Fatal(err)
	}
	orig := writeLog
	t.Cleanup(func() { writeLog = orig })
	errFull := errors.New("no space left on device")
	writeLog = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, errFull
	}
	j.put("b", "2")
	j.put("c", "3")
	if err := j.commit(); !errors.Is(err, errFull) {
		t.Fatalf("commit on a full disk: %v", err)
	}
	writeLog = orig
	j.put("d", "4")
	if err := j.commit(); err != nil {
		t.Fatal(err)
	}
	j.close()
	_, got := openTestJournal(t, path)
	if got["a"] != "1" || got["d"] != "4" || len(got) != 2 {
		t.Errorf("after a batch the disk cut short and one after it: %v, want a and d", got)
	}
}

// TestJournalDropsATornTail checks a log cut short mid-line (a crash during
// the append) loses only that line, and the next append starts cleanly.
func TestJournalDropsATornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.json")
	j, _ := openTestJournal(t, path)
	j.put("a", "1")
	j.commit()
	j.close()
	f, _ := os.OpenFile(path+".log", os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"q":2,"k":"b","v":"tor`)
	f.Close()

	j, got := openTestJournal(t, path)
	if got["a"] != "1" || len(got) != 1 {
		t.Fatalf("with a torn tail: %v", got)
	}
	j.put("c", "3")
	j.commit()
	j.close()
	_, got = openTestJournal(t, path)
	if got["a"] != "1" || got["c"] != "3" || len(got) != 2 {
		t.Errorf("after appending past a torn tail: %v", got)
	}
}

// TestJournalSkipsLinesItsSnapshotHolds checks a crash between writing a
// snapshot and emptying the log does not replay old lines over it.
func TestJournalSkipsLinesItsSnapshotHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.json")
	j, _ := openTestJournal(t, path)
	j.put("a", "old")
	j.commit()
	log, _ := os.ReadFile(path + ".log")
	j.compact(map[string]any{"a": "new"})
	j.close()
	// The crash: the log still holds what the snapshot folded in.
	os.WriteFile(path+".log", log, 0o600)
	_, got := openTestJournal(t, path)
	if got["a"] != "new" {
		t.Errorf("a = %q, want the snapshot's", got["a"])
	}
}

// TestJournalOfANewerFleet checks a snapshot of a newer version is refused
// and left as it is, and a corrupt one is moved aside for a fresh start.
func TestJournalOfANewerFleet(t *testing.T) {
	dir := t.TempDir()
	newer := filepath.Join(dir, "newer.json")
	os.WriteFile(newer, []byte(`{"v":99,"seq":1}`+"\n"), 0o600)
	if _, _, err := openJournal(newer); !errors.Is(err, ErrNewerState) {
		t.Errorf("a newer snapshot: %v, want ErrNewerState", err)
	}
	if data, _ := os.ReadFile(newer); !strings.Contains(string(data), `"v":99`) {
		t.Error("the newer snapshot was rewritten")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	os.WriteFile(corrupt, []byte("not json\n"), 0o600)
	j, got := openTestJournal(t, corrupt)
	j.close()
	if len(got) != 0 {
		t.Errorf("a corrupt snapshot gave %v", got)
	}
	aside, _ := filepath.Glob(corrupt + ".corrupt-*")
	if len(aside) != 1 {
		t.Errorf("corrupt snapshot moved aside as %v", aside)
	}
}

// FuzzJournalOpen checks no snapshot or log content makes opening a journal
// panic, and a journal that opens can be written.
func FuzzJournalOpen(f *testing.F) {
	f.Add([]byte(`{"v":1,"seq":2}`+"\n"+`{"k":"a","v":"1"}`+"\n"), []byte(`{"q":3,"k":"b","v":"2"}`+"\n"))
	f.Add([]byte(""), []byte(`{"q":1,"k":"a","d":true}`))
	f.Add([]byte(`{"v":1}`), []byte("\n\n{"))
	f.Fuzz(func(t *testing.T, snapshot, log []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "root.json")
		os.WriteFile(path, snapshot, 0o600)
		os.WriteFile(path+".log", log, 0o600)
		j, _, err := openJournal(path)
		if err != nil {
			return
		}
		j.put("x", "y")
		if err := j.commit(); err != nil {
			t.Fatalf("commit after open: %v", err)
		}
		j.close()
		j, raw, err := openJournal(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		j.close()
		if string(raw["x"]) != `"y"` {
			t.Fatalf("record written after open lost: %q", raw["x"])
		}
	})
}
