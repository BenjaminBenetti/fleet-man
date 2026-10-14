package sharesync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// docCase is a change to a settings file meeting the hub's state.
type docCase struct {
	// base is the content the change was made on, "" for none kept.
	base     string
	cur, inc string
	// curNewer makes the current copy the newer change.
	curNewer bool
	fast     bool
	path     string
}

// input returns the case as the hub hands it to a policy: the current copy
// written at 100 (300 when curNewer), the incoming at 200.
func (c docCase) input() *MergeInput {
	curT, incT := int64(100), int64(200)
	if c.curNewer {
		curT = 300
	}
	cur := version(c.cur, curT, "a")
	name := c.path
	if name == "" {
		name = "settings.json"
	}
	in := &MergeInput{Root: "tool", Path: name, Side: SideInfo{ID: "b", Location: "k1"}, Fast: c.fast, Current: &cur, Incoming: version(c.inc, incT, "b"), BaseRev: 3}
	if c.base != "" {
		base := c.base
		in.base = func(context.Context) ([]byte, bool, error) { return []byte(base), true, nil }
	}
	return in
}

// TestJSON3Decisions pins json3's table: fast forwards taken as they are,
// '{}' never replacing content, a file that does not parse left waiting,
// and two changed copies merged three ways (both sides' keys, lists as
// sets, a change beating a removal, the newer change winning a value both
// changed), falling back to the newer copy only for a merge to '{}'.
func TestJSON3Decisions(t *testing.T) {
	cases := []struct {
		name     string
		c        docCase
		decision Decision
		want     string
		reason   string
	}{
		{"fast forward", docCase{cur: `{"a":1}`, inc: `{"a":2}`, fast: true}, Accept, "", ""},
		{"fast forward of an unreadable current", docCase{cur: `{"a":`, inc: `{"a":2}`, fast: true}, Accept, "", ""},
		{"'{}' never replaces content, even fast", docCase{cur: `{"a":1}`, inc: `{}`, fast: true}, Keep, "", "never replaces"},
		{"'{}' spelt with space and a newline", docCase{cur: `{"a":1}`, inc: "{ }\n"}, Keep, "", "never replaces"},
		{"content replaces '{}'", docCase{cur: `{}`, inc: `{"a":1}`}, Accept, "", "replaces an empty"},
		{"content replaces an unreadable current", docCase{cur: `{"a":`, inc: `{"a":1}`}, Accept, "", "replaces an empty or unreadable"},
		{"both changed other keys", docCase{base: `{"a":1,"b":1}`, cur: `{"a":2,"b":1}`, inc: `{"a":1,"b":2}`}, Merged, `{"a":2,"b":2}`, "merged"},
		{"both added keys", docCase{base: `{"a":1}`, cur: `{"a":1,"c":3}`, inc: `{"a":1,"b":2}`}, Merged, `{"a":1,"c":3,"b":2}`, ""},
		{"the newer value of one both changed", docCase{base: `{"m":"x","k":1}`, cur: `{"m":"y","k":1}`, inc: `{"m":"z","k":2}`}, Accept, "", "newer change's m kept"},
		{"the newer current value kept", docCase{base: `{"m":"x","k":1}`, cur: `{"m":"y","k":1}`, inc: `{"m":"z","k":2}`, curNewer: true}, Merged, `{"m":"y","k":2}`, "newer change's m kept"},
		{"a change beats a removal", docCase{base: `{"a":1,"b":1}`, cur: `{"b":1,"c":1}`, inc: `{"a":2,"b":1}`}, Merged, `{"b":1,"c":1,"a":2}`, ""},
		{"a removal of an unchanged key", docCase{base: `{"a":1,"b":1,"c":1}`, cur: `{"a":1,"b":1}`, inc: `{"a":1,"b":1,"c":1,"d":1}`}, Merged, `{"a":1,"b":1,"d":1}`, ""},
		{"nested objects merge", docCase{base: `{"p":{"x":1,"y":1}}`, cur: `{"p":{"x":2,"y":1}}`, inc: `{"p":{"x":1,"y":2}}`}, Merged, `{"p":{"x":2,"y":2}}`, ""},
		{"hook arrays are sets", docCase{
			base: `{"hooks":{"Stop":[{"matcher":"*","hooks":[{"command":"a"}]}]}}`,
			cur:  `{"hooks":{"Stop":[{"matcher":"*","hooks":[{"command":"a"}]},{"matcher":"*","hooks":[{"command":"b"}]}]}}`,
			inc:  `{"hooks":{"Stop":[{"hooks":[{"command":"a"}],"matcher":"*"},{"matcher":"*","hooks":[{"command":"c"}]}]}}`,
		}, Merged, `{"hooks":{"Stop":[{"matcher":"*","hooks":[{"command":"a"}]},{"matcher":"*","hooks":[{"command":"b"}]},{"matcher":"*","hooks":[{"command":"c"}]}]}}`, ""},
		{"the same hook added on both", docCase{base: `{"h":[]}`, cur: `{"h":["x"],"a":1}`, inc: `{"h":["x"],"b":1}`}, Merged, `{"h":["x"],"a":1,"b":1}`, ""},
		{"a set removal holds", docCase{base: `{"allow":["a","b"]}`, cur: `{"allow":["a"]}`, inc: `{"allow":["a","b","c"]}`}, Merged, `{"allow":["a","c"]}`, ""},
		{"no base: both copies' keys kept", docCase{cur: `{"a":1}`, inc: `{"b":2}`}, Merged, `{"a":1,"b":2}`, ""},
		{"no base: a removal cannot be told", docCase{cur: `{"a":1}`, inc: `{"a":1,"b":2}`}, Accept, "", ""},
		{"a merge to '{}' keeps the newer copy", docCase{base: `{"a":1,"b":1}`, cur: `{"a":1}`, inc: `{"b":1}`}, Accept, "", "would merge to an empty"},
		{"a merge to the current copy", docCase{base: `{"a":1}`, cur: `{"a":1,"b":2}`, inc: `{"a":1,"b":2}`}, Keep, "", ""},
		{"numbers by value", docCase{base: `{"n":1,"x":0}`, cur: `{"n":1.0,"x":1}`, inc: `{"n":1,"x":0,"y":1}`}, Merged, `{"n":1.0,"x":1,"y":1}`, ""},
		{"strings by value", docCase{base: `{"s":"é","x":0}`, cur: `{"s":"\u00e9","x":1}`, inc: `{"s":"é","x":0,"y":1}`}, Merged, `{"s":"\u00e9","x":1,"y":1}`, ""},
	}
	ctx := context.Background()
	for _, tc := range cases {
		res, err := JSON3{}.Merge(ctx, tc.c.input())
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if res.Decision != tc.decision || res.Conflict && tc.decision != Accept {
			t.Errorf("%s: decision %d (conflict %v), want %d", tc.name, res.Decision, res.Conflict, tc.decision)
		}
		if tc.want != "" && string(res.Content) != tc.want {
			t.Errorf("%s: merged %s, want %s", tc.name, res.Content, tc.want)
		}
		if tc.reason != "" && !strings.Contains(res.Reason, tc.reason) {
			t.Errorf("%s: reason %q, want it to say %q", tc.name, res.Reason, tc.reason)
		}
	}
}

// TestJSON3LeavesAWriteUnderWayWaiting checks a file that does not parse
// (empty, cut short, saved mid-edit) is never taken, fast or not: the
// change waits on its side (ErrIncomplete) until a whole one is written.
func TestJSON3LeavesAWriteUnderWayWaiting(t *testing.T) {
	for _, inc := range []string{"", "  \n", `{"a":`, `{"a":1}}`, `{"a":1,}`, "{\"a\":1}\n{"} {
		for _, fast := range []bool{true, false} {
			_, err := JSON3{}.Merge(context.Background(), docCase{cur: `{"a":0}`, inc: inc, fast: fast}.input())
			if !errors.Is(err, ErrIncomplete) {
				t.Errorf("%q (fast %v): %v, want ErrIncomplete", inc, fast, err)
			}
		}
	}
	// With nothing at the path yet, too.
	in := docCase{inc: `{"a":`}.input()
	in.Current = nil
	if _, err := (JSON3{}).Merge(context.Background(), in); !errors.Is(err, ErrIncomplete) {
		t.Errorf("a first copy cut short: %v, want ErrIncomplete", err)
	}
}

// TestJSON3DeletesAndKindsGoAsMirror checks what is not two files decides
// as mirror: a fast delete goes, a stale delete is refused, '{}' arriving
// where nothing is is taken.
func TestJSON3DeletesAndKindsGoAsMirror(t *testing.T) {
	ctx := context.Background()
	gone := Version{Entry: Entry{Kind: Gone}, Side: "b"}
	in := docCase{cur: `{"a":1}`}.input()
	in.Incoming, in.Fast = gone, true
	if res, _ := (JSON3{}).Merge(ctx, in); res.Decision != Accept {
		t.Errorf("a fast delete: %+v", res)
	}
	in.Fast = false
	if res, _ := (JSON3{}).Merge(ctx, in); res.Decision != Keep {
		t.Errorf("a stale delete: %+v", res)
	}
	in = docCase{inc: `{}`}.input()
	in.Current = nil
	if res, err := (JSON3{}).Merge(ctx, in); err != nil || res.Decision != Accept {
		t.Errorf("'{}' where nothing is: %+v, %v", res, err)
	}
	in = docCase{inc: `{}`}.input()
	in.Current = &Version{Entry: Entry{Kind: Gone}, Rev: 4}
	if res, err := (JSON3{}).Merge(ctx, in); err != nil || res.Decision != Accept {
		t.Errorf("'{}' after a delete it had not seen: %+v, %v", res, err)
	}
	big := docCase{cur: `{"a":1}`, inc: `{"a":2}`}.input()
	big.Incoming.Entry.Size = MaxMergeSize + 1
	if res, err := (JSON3{}).Merge(ctx, big); err != nil || res.Decision != Accept || !res.Conflict {
		t.Errorf("too large to merge: %+v, %v, want mirror's newer-wins", res, err)
	}
	failing := docCase{base: `{}`, cur: `{"a":1}`, inc: `{"b":2}`}.input()
	failing.Current.open = func(context.Context) (io.ReadCloser, error) { return nil, ErrNoSource }
	if _, err := (JSON3{}).Merge(ctx, failing); !errors.Is(err, ErrNoSource) {
		t.Errorf("unreachable current copy: %v, want the error (the change waits)", err)
	}
}

// TestJSON3KeepsTheFormat checks the merged file is written as the current
// copy is: its key order, its indent (JSON.stringify's two spaces for
// Claude Code, tabs, or compact), its escapes and its final newline.
func TestJSON3KeepsTheFormat(t *testing.T) {
	// The current copy changed z; the incoming one added a key: neither
	// copy is the merge.
	cases := []struct{ name, cur, inc, want string }{
		{
			"two spaces, no final newline (Claude Code)",
			"{\n  \"z\": 2,\n  \"a\": [\n    \"x\"\n  ],\n  \"e\": {},\n  \"s\": \"\\u00e9\\n\"\n}",
			`{"z":1,"a":["x"],"e":{},"s":"\u00e9\n","new":true}`,
			"{\n  \"z\": 2,\n  \"a\": [\n    \"x\"\n  ],\n  \"e\": {},\n  \"s\": \"\\u00e9\\n\",\n  \"new\": true\n}",
		},
		{"tabs and a final newline", "{\n\t\"z\": 2\n}\n", `{"z":1,"new":true}`, "{\n\t\"z\": 2,\n\t\"new\": true\n}\n"},
		{"compact", `{"z":2}`, `{"z":1,"new":true}`, `{"z":2,"new":true}`},
		{"compact with a final newline", "{\"z\":2}\n", `{"z":1,"new":true}`, "{\"z\":2,\"new\":true}\n"},
	}
	for _, tc := range cases {
		base := `{"z":1,"a":["x"],"e":{},"s":"\u00e9\n"}`
		if !strings.Contains(tc.cur, `"a"`) {
			base = `{"z":1}`
		}
		res, err := JSON3{}.Merge(context.Background(), docCase{base: base, cur: tc.cur, inc: tc.inc}.input())
		if err != nil || res.Decision != Merged {
			t.Errorf("%s: %+v, %v", tc.name, res, err)
			continue
		}
		if string(res.Content) != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, res.Content, tc.want)
		}
	}
}

// TestParseJSONKeepsTheSpelling checks the parser keeps each token as
// spelt and an object's first place for a repeated key, with its last
// value (as JSON.parse does).
func TestParseJSONKeepsTheSpelling(t *testing.T) {
	for _, doc := range []string{
		`{"a":1,"b":[1,2.50,-3e2,true,false,null],"c":{"d":"\u0041\"\\"},"":{}}`,
		`[]`, `"x"`, `12`, `null`, `[{},[],""]`,
	} {
		v, err := parseJSON([]byte(" \n" + doc + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		var b bytes.Buffer
		writeJSON(&b, v)
		if b.String() != doc {
			t.Errorf("round trip of %s gave %s", doc, b.String())
		}
	}
	v, _ := parseJSON([]byte(`{"a":1,"b":2,"a":3}`))
	if strings.Join(v.keys, ",") != "a,b" || v.fields["a"].text != "3" {
		t.Errorf("repeated key: keys %v, a = %s", v.keys, v.fields["a"].text)
	}
	x, _ := parseJSON([]byte(`{"a":[1,{"b":2}],"c":"é"}`))
	y, _ := parseJSON([]byte("{\"c\":\"\\u00e9\",\n \"a\":[1.0,{\"b\":2e0}]}"))
	if !same(x, y) {
		t.Error("the same value spelt two ways differs")
	}
	z, _ := parseJSON([]byte(`{"a":[{"b":2},1],"c":"é"}`))
	if same(x, z) {
		t.Error("arrays in another order are the same value")
	}
}
