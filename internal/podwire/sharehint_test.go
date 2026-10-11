package podwire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestShareHintRoundTrip: hints written are read back; more paths than a
// hint carries go as the whole folder.
func TestShareHintRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteShareHint(&buf, ShareHintMsg{Root: ".claude", Paths: []string{"projects/a/s.jsonl", "history.jsonl"}}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxHintPaths+1)
	for i := range many {
		many[i] = "f"
	}
	if err := WriteShareHint(&buf, ShareHintMsg{Root: ".mnt/data", Paths: many}); err != nil {
		t.Fatal(err)
	}
	r := NewShareHintReader(&buf)
	m, err := r.Next()
	if err != nil || m.Root != ".claude" || len(m.Paths) != 2 || m.Paths[0] != "projects/a/s.jsonl" {
		t.Fatalf("first hint %+v %v", m, err)
	}
	m, err = r.Next()
	if err != nil || m.Root != ".mnt/data" || len(m.Paths) != 1 || m.Paths[0] != "." {
		t.Fatalf("an oversized hint %+v %v, want the whole folder", m, err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last hint: %v", err)
	}
}

// TestShareHintRefusesWhatIsNotOne: a hint for a path outside its folder, an
// absolute one, an empty hint, a line over the limit, or a half line is an
// error.
func TestShareHintRefusesWhatIsNotOne(t *testing.T) {
	for name, line := range map[string]string{
		"escape":    `{"root":".claude","paths":["../x"]}` + "\n",
		"absolute":  `{"root":".claude","paths":["/etc/passwd"]}` + "\n",
		"root up":   `{"root":"..","paths":["x"]}` + "\n",
		"root dot":  `{"root":".","paths":["x"]}` + "\n",
		"no paths":  `{"root":".claude","paths":[]}` + "\n",
		"not json":  "hello\n",
		"half line": `{"root":".claude","paths":["x"]}`,
		"too long":  `{"root":".claude","paths":["` + strings.Repeat("a", maxHintLine) + `"]}` + "\n",
	} {
		if _, err := NewShareHintReader(strings.NewReader(line)).Next(); err == nil {
			t.Errorf("%s: read as a hint", name)
		}
	}
}

// TestDesiredStateValidatesTheShareHint: a desired state naming a share
// folder that is not a clean path, or naming one twice, is refused.
func TestDesiredStateValidatesTheShareHint(t *testing.T) {
	good := DesiredState{ShareHint: &ShareHint{Roots: []ShareHintRoot{{Name: ".claude"}, {Name: ".config/gh"}, {Name: ".mnt", Skip: []string{"opt/data"}}}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good share hint: %v", err)
	}
	for name, h := range map[string]*ShareHint{
		"escape":    {Roots: []ShareHintRoot{{Name: "../x"}}},
		"twice":     {Roots: []ShareHintRoot{{Name: ".claude"}, {Name: ".claude"}}},
		"bad skip":  {Roots: []ShareHintRoot{{Name: ".mnt", Skip: []string{"/abs"}}}},
		"the roots": {Roots: []ShareHintRoot{{Name: "."}}},
	} {
		if err := (DesiredState{ShareHint: h}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
