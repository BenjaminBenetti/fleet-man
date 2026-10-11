package sharesync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// version returns a file version holding content.
func version(content string, mtime int64, side string) Version {
	return Version{
		Entry: Entry{Kind: File, Mode: 0o644, Size: int64(len(content)), MTime: mtime, Hash: hashOf([]byte(content))},
		Side:  side,
		open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		},
	}
}

// TestMirrorDecisions pins mirror's table: a fast forward is taken, the
// newer of two concurrent changes wins with a conflict copy of the other, a
// delete never wins over a change it did not see, a change made after a
// delete it did not see brings the path back, and equal times are decided
// the same whichever side reports.
func TestMirrorDecisions(t *testing.T) {
	older, newer := version("old", 100, "a"), version("new", 200, "b")
	gone := Version{Entry: Entry{Kind: Gone, Mode: 0o644}, Side: "c"}
	cases := []struct {
		name     string
		fast     bool
		cur      *Version
		inc      Version
		decision Decision
		conflict bool
	}{
		{"new path", false, nil, older, Accept, false},
		{"fast forward", true, &newer, older, Accept, false},
		{"fast delete", true, &newer, gone, Accept, false},
		{"newer change wins", false, &older, newer, Accept, true},
		{"older change loses", false, &newer, older, Keep, true},
		{"stale delete loses", false, &newer, gone, Keep, false},
		{"change after an unseen delete", false, &gone, older, Accept, false},
	}
	for _, tc := range cases {
		res, err := Mirror{}.Merge(context.Background(), &MergeInput{Path: "f", Fast: tc.fast, Current: tc.cur, Incoming: tc.inc})
		if err != nil || res.Decision != tc.decision || res.Conflict != tc.conflict {
			t.Errorf("%s: %+v, %v; want decision %d conflict %v", tc.name, res, err, tc.decision, tc.conflict)
		}
	}
	a, b := version("aaa", 100, "a"), version("bbb", 100, "b")
	ab, _ := Mirror{}.Merge(context.Background(), &MergeInput{Current: &a, Incoming: b})
	ba, _ := Mirror{}.Merge(context.Background(), &MergeInput{Current: &b, Incoming: a})
	winAB := map[Decision]string{Accept: "bbb", Keep: "aaa"}[ab.Decision]
	winBA := map[Decision]string{Accept: "aaa", Keep: "bbb"}[ba.Decision]
	if winAB != winBA {
		t.Errorf("a tie is decided by arrival: %s vs %s", winAB, winBA)
	}
}

// TestUnionLines pins append-union's merge.
func TestUnionLines(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "", ""},
		{"x\n", "", "x\n"},
		{"", "x\n", "x\n"},
		{"x\n", "x\n", "x\n"},
		{"a\nb\n", "a\nc\n", "a\nb\nc\n"},
		{"a\nb\n", "b\na\n", "a\nb\n"},
		// A multiset: a line the other copy has twice is added once more.
		{"a\n", "a\na\n", "a\na\n"},
		// A line still being written that the other copy completed is kept
		// only completed.
		{"a\n{\"x\":", "a\n{\"x\":1}\n", "a\n{\"x\":1}\n"},
		{"a\n{\"x\":1}\n", "a\n{\"x\":", "a\n{\"x\":1}\n"},
		// Two unfinished lines, one further along: the longer stays open.
		{"a\n{\"x", "a\n{\"x\":1", "a\n{\"x\":1"},
		// A finished copy without a final newline gets one before more lines.
		{"a", "b\n", "a\nb\n"},
		{"a\n", "b", "a\nb"},
	}
	for _, tc := range cases {
		if got := string(unionLines([]byte(tc.a), []byte(tc.b))); got != tc.want {
			t.Errorf("unionLines(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// FuzzUnionLines checks the union keeps every complete line of both copies
// (as a multiset), starts with the first copy's complete lines, and is
// stable: merging either copy into it again changes nothing.
func FuzzUnionLines(f *testing.F) {
	f.Add([]byte("a\nb\n"), []byte("a\nc\n"))
	f.Add([]byte("a\n{\"x\":"), []byte("{\"x\":1}\n"))
	f.Add([]byte(""), []byte("\n\n"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		got := unionLines(a, b)
		for _, src := range [][]byte{a, b} {
			lines, open := splitLines(src)
			if open {
				lines = lines[:len(lines)-1]
			}
			count := map[string]int{}
			for _, line := range lines {
				count[line]++
			}
			have := map[string]int{}
			gotLines, gotOpen := splitLines(got)
			if gotOpen {
				gotLines = gotLines[:len(gotLines)-1]
			}
			for _, line := range gotLines {
				have[line]++
			}
			for line, n := range count {
				if have[line] < n {
					t.Fatalf("union(%q, %q) = %q lost line %q", a, b, got, line)
				}
			}
		}
		if again := unionLines(got, a); !bytes.Equal(again, got) && bytes.HasSuffix(got, []byte("\n")) {
			t.Fatalf("union(%q, %q) = %q, and again with a: %q", a, b, got, again)
		}
		if again := unionLines(got, b); !bytes.Equal(again, got) && bytes.HasSuffix(got, []byte("\n")) {
			t.Fatalf("union(%q, %q) = %q, and again with b: %q", a, b, got, again)
		}
	})
}

// TestAppendUnionDecisions checks append-union decides like mirror except
// for two concurrent files, which merge, and falls back to mirror for what
// it cannot read.
func TestAppendUnionDecisions(t *testing.T) {
	ctx := context.Background()
	cur, inc := version("a\nb\n", 100, "x"), version("a\nc\n", 50, "y")
	res, err := AppendUnion{}.Merge(ctx, &MergeInput{Current: &cur, Incoming: inc})
	if err != nil || res.Decision != Merged || string(res.Content) != "a\nb\nc\n" || res.Conflict {
		t.Errorf("concurrent appends: %+v, %v", res, err)
	}
	sub := version("a\n", 300, "y")
	if res, _ := (AppendUnion{}).Merge(ctx, &MergeInput{Current: &cur, Incoming: sub}); res.Decision != Keep {
		t.Errorf("a copy whose lines are all there: %+v, want Keep", res)
	}
	sup := version("a\nb\nc\n", 1, "y")
	if res, _ := (AppendUnion{}).Merge(ctx, &MergeInput{Current: &cur, Incoming: sup}); res.Decision != Accept {
		t.Errorf("a copy holding every line: %+v, want Accept", res)
	}
	gone := Version{Entry: Entry{Kind: Gone}}
	if res, _ := (AppendUnion{}).Merge(ctx, &MergeInput{Current: &cur, Incoming: gone}); res.Decision != Keep {
		t.Errorf("a stale delete: %+v, want Keep", res)
	}
	if res, _ := (AppendUnion{}).Merge(ctx, &MergeInput{Fast: true, Current: &cur, Incoming: sub}); res.Decision != Accept {
		t.Errorf("a fast forward (a deliberate truncation): %+v, want Accept", res)
	}
	big := cur
	big.Entry.Size = MaxMergeSize + 1
	big.Entry.MTime = 999
	if res, _ := (AppendUnion{}).Merge(ctx, &MergeInput{Current: &cur, Incoming: big}); res.Decision != Accept || !res.Conflict {
		t.Errorf("too large to merge: %+v, want mirror's newer-wins", res)
	}
	failing := version("a\nb\n", 100, "x")
	failing.open = func(context.Context) (io.ReadCloser, error) { return nil, ErrNoSource }
	if _, err := (AppendUnion{}).Merge(ctx, &MergeInput{Current: &failing, Incoming: inc}); !errors.Is(err, ErrNoSource) {
		t.Errorf("unreadable content: %v, want the error (the change waits)", err)
	}
}

// TestPoliciesCoverTheTable checks a hub refuses a root whose policies are
// not all implemented, and the defaults implement every policy the
// defaults' roots use.
func TestPoliciesCoverTheTable(t *testing.T) {
	for _, root := range syncpolicy.Defaults().Roots() {
		if err := DefaultPolicies().covers(syncpolicy.Defaults(), root.Path); err != nil {
			t.Errorf("%s with the default policies: %v", root.Path, err)
		}
	}
	partial := DefaultPolicies()
	delete(partial, syncpolicy.Credential)
	err := partial.covers(syncpolicy.Defaults(), ".claude")
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf(".claude without a credential policy: %v, want it named", err)
	}
	if err := partial.covers(syncpolicy.Defaults(), ".mnt"); err != nil {
		t.Errorf(".mnt: %v", err)
	}
	if _, err := OpenHub(t.TempDir(), syncpolicy.Table{}, []string{".claude"}, HubOptions{Policies: partial}); err == nil {
		t.Error("OpenHub took .claude without a credential policy")
	}
	h, err := OpenHub(t.TempDir(), syncpolicy.Table{}, []string{".claude", ".codex", ".config/gh", ".augment", "files", ".mnt", ".hostbind"}, HubOptions{})
	if err != nil {
		t.Fatalf("OpenHub with the defaults: %v", err)
	}
	h.Close()
}

// TestEntryCheck refuses entries no scan reports.
func TestEntryCheck(t *testing.T) {
	good := Entry{Kind: File, Mode: 0o644, Size: 3, Hash: hashOf([]byte("abc"))}
	bad := []Entry{
		{Kind: File, Mode: 0o4755, Hash: good.Hash},
		{Kind: File, Mode: 0o644, Size: -1, Hash: good.Hash},
		{Kind: File, Mode: 0o644, Hash: "short"},
		{Kind: File, Mode: 0o644, Hash: strings.ToUpper(good.Hash)},
		{Kind: File, Mode: 0o644, Size: 3, Hash: good.Hash, Grew: []Prefix{{Size: 3, Hash: good.Hash}}},
		{Kind: Dir, Mode: 0o2755},
		{Kind: Symlink},
		{Kind: Symlink, Target: "a\x00b"},
		{Kind: 9},
	}
	if err := good.check(); err != nil {
		t.Errorf("good entry: %v", err)
	}
	for _, e := range bad {
		if e.check() == nil {
			t.Errorf("check(%+v) passed", e)
		}
	}
	for _, p := range []string{"..", "../x", "/abs", "a//b", "a/./b", ".", "", "a\x00b"} {
		if validPath(p) {
			t.Errorf("validPath(%q) passed", p)
		}
	}
}

// TestFleetDir refuses fleet names that would leave ~/.fleet/sync.
func TestFleetDir(t *testing.T) {
	if dir, err := FleetDir("/home/u/.fleet/sync", "alpha"); err != nil || dir != "/home/u/.fleet/sync/alpha" {
		t.Errorf("FleetDir(alpha) = %q, %v", dir, err)
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a b", "a\x00"} {
		if _, err := FleetDir("/base", name); err == nil {
			t.Errorf("FleetDir(%q) passed", name)
		}
	}
}
