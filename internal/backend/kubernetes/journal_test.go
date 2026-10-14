package kubernetes

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const testKey = "k7sk7sk7"

func testJournal() Journal {
	return Journal{Op: OpCreate, Fleet: "alpha", Instance: "one", Cluster: "c1-journal", Namespace: "fleet", Key: testKey, Gen: 1, Step: "preflight"}
}

// A job's journal is on disk (0600 in a 0700 dir) after every change, and
// gone once the job finished.
func TestJournalLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kube", "jobs")
	path := filepath.Join(dir, testKey+".json")
	jj, err := BeginJournalAt(path, testJournal())
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("journal file %v %v", fi, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("journal dir %v %v", fi, err)
	}
	read := func() Journal {
		t.Helper()
		j, err := ReadJournal(path)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	if j := read(); j.V != JournalVersion || j.StartedAt.IsZero() || j.Mark() != "create@preflight" {
		t.Fatalf("begun journal %+v", j)
	}

	if err := jj.Intend(JournalObject{Kind: KindPVC, Name: "fi-alpha-one-" + testKey, Created: true, Anchor: true}); err != nil {
		t.Fatal(err)
	}
	if j := read(); len(j.Objects) != 1 || j.Objects[0].UID != "" {
		t.Fatalf("an intended write is recorded before it happens: %+v", j.Objects)
	}
	if err := jj.Applied(KindPVC, "fi-alpha-one-"+testKey, "uid-pvc"); err != nil {
		t.Fatal(err)
	}
	// Intending the same object again (a re-apply) keeps its UID.
	if err := jj.Intend(JournalObject{Kind: KindPVC, Name: "fi-alpha-one-" + testKey, Created: true, Anchor: true}); err != nil {
		t.Fatal(err)
	}
	if err := jj.Step("seed"); err != nil {
		t.Fatal(err)
	}
	j := read()
	if a, ok := j.Anchor(); !ok || a.UID != "uid-pvc" || !a.Created {
		t.Fatalf("anchor %+v %v", a, ok)
	}
	if j.Step != "seed" || j.PrevStep != "preflight" || jj.Mark() != "create@seed" {
		t.Fatalf("steps %q after %q, mark %q", j.Step, j.PrevStep, jj.Mark())
	}
	if !j.Current("create@seed") || !j.Current("create@preflight") || j.Current("create@apply") || j.Current("rebuild@seed") || j.Current("") {
		t.Error("a journal is current for its step and the one before, nothing else")
	}
	if err := jj.Step("Bad Step"); err == nil {
		t.Error("a step that is no annotation value was journaled")
	}

	if err := jj.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a finished job's journal is still there: %v", err)
	}
	if err := jj.Step("late"); err == nil {
		t.Error("a finished journal took a step")
	}
}

func TestBeginJournalRefusesAnUnreconciledOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), testKey+".json")
	if _, err := BeginJournalAt(path, testJournal()); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginJournalAt(path, testJournal()); !errors.Is(err, ErrJournalExists) {
		t.Fatalf("a second job on the key: %v", err)
	}
}

func TestJournalValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Journal){
		"unknown op":    func(j *Journal) { j.Op = "resume" },
		"bad key":       func(j *Journal) { j.Key = "../../x" },
		"no cluster":    func(j *Journal) { j.Cluster = "" },
		"no instance":   func(j *Journal) { j.Instance = "" },
		"bad step":      func(j *Journal) { j.Step = "a@b" },
		"nameless":      func(j *Journal) { j.Objects = []JournalObject{{Kind: KindPVC}} },
		"newer version": func(j *Journal) { j.V = JournalVersion + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			j := testJournal()
			j.V = JournalVersion
			mutate(&j)
			if err := j.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A rebuild that reached its reset says so for good, whatever its later
// steps are.
func TestJournalResetIsSticky(t *testing.T) {
	j := testJournal()
	j.Op = OpRebuild
	jj, err := BeginJournalAt(filepath.Join(t.TempDir(), testKey+".json"), j)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []Step{"export", "build", "scale-down"} {
		if err := jj.Step(step); err != nil {
			t.Fatal(err)
		}
	}
	if jj.Journal().ResetStarted {
		t.Fatal("reset before its step")
	}
	for _, step := range []Step{StepReset, "seed", "apply", "hooks"} {
		if err := jj.Step(step); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadJournal(jj.Path())
	if err != nil || !got.ResetStarted {
		t.Fatalf("after its reset: %+v %v", got, err)
	}
}

// A journal a newer fleet wrote is reported, never read as this one's.
func TestReadJournalNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), testKey+".json")
	j := testJournal()
	j.V = JournalVersion + 1
	data, _ := json.Marshal(j)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadJournal(path)
	if !errors.Is(err, ErrJournalNewer) || got.Key != testKey {
		t.Fatalf("newer journal: %+v %v", got, err)
	}
}

func TestListJournals(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, j Journal) {
		t.Helper()
		j.V = JournalVersion
		data, _ := json.Marshal(j)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	older, newer := testJournal(), testJournal()
	older.Key, older.StartedAt = "aaaaaaaa", time.Unix(1000, 0)
	newer.Key, newer.StartedAt = "bbbbbbbb", time.Unix(2000, 0)
	write("bbbbbbbb.json", newer)
	write("aaaaaaaa.json", older)
	write("cccccccc.json", newer) // names another key
	write(".bbbbbbbb.json-123.tmp", newer)
	if err := os.WriteFile(filepath.Join(dir, "dddddddd.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := ListJournals(dir)
	if err != nil {
		t.Fatal(err)
	}
	var good, bad []string
	for _, f := range files {
		if f.Err != nil {
			bad = append(bad, filepath.Base(f.Path))
		} else {
			good = append(good, f.Journal.Key)
		}
	}
	if strings.Join(good, ",") != "aaaaaaaa,bbbbbbbb" || len(bad) != 2 {
		t.Fatalf("good %v (oldest first), bad %v", good, bad)
	}
	if files, err := ListJournals(filepath.Join(dir, "missing")); err != nil || len(files) != 0 {
		t.Fatalf("a missing dir: %v %v", files, err)
	}
}

func TestOpStatuses(t *testing.T) {
	for _, op := range []Op{OpCreate, OpClone, OpRebuild} {
		got, ok := OpForStatus(op.Status())
		if !ok || got != op {
			t.Errorf("%s: status %s maps back to %s", op, op.Status(), got)
		}
	}
	if _, ok := OpForStatus(fleet.StatusRunning); ok {
		t.Error("running is no job's status")
	}
	if !names.ValidHash(testKey) {
		t.Fatal("test key is not a key")
	}
}
