package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// journal.go is the intent journal of a Kubernetes create, clone or rebuild:
// ~/.fleet/kube/jobs/<key8>.json, written before each of the job's API
// server writes, so that a daemon that dies mid-job leaves a record of every
// object the job made or changed and of how far it got. The next daemon's
// reconciler reads it (internal/server/kube_reconcile.go) and fails or rolls
// the job back; it never resumes one. The job's current step is mirrored on
// its anchor object as the fleet.dev/op annotation ("<op>@<step>"), which is
// how the reconciler tells a journal still describing the cluster from one a
// backup restored after its job had finished. A job that ends, well or
// badly, removes both.
//
// The file is versioned: one written by a newer fleet is never acted on, only
// reported, and an older fleet's is read with this one's rules.

// JournalVersion is the journal format this fleet writes and reads.
const JournalVersion = 1

// Op is the kind of job a journal records.
type Op string

const (
	OpCreate  Op = "create"
	OpClone   Op = "clone"
	OpRebuild Op = "rebuild"
)

// Valid reports whether o is an op this fleet knows.
func (o Op) Valid() bool { return o == OpCreate || o == OpClone || o == OpRebuild }

// Status is the transitional status a record has while a job of op runs.
func (o Op) Status() fleet.InstanceStatus {
	switch o {
	case OpClone:
		return fleet.StatusCloning
	case OpRebuild:
		return fleet.StatusRebuilding
	}
	return fleet.StatusCreating
}

// OpForStatus is the op whose job leaves a record in status, if any.
func OpForStatus(s fleet.InstanceStatus) (Op, bool) {
	switch s {
	case fleet.StatusCreating:
		return OpCreate, true
	case fleet.StatusCloning:
		return OpClone, true
	case fleet.StatusRebuilding:
		return OpRebuild, true
	}
	return "", false
}

// Step names how far a job got. Steps are the job's own words (preflight,
// seed, apply, ...), lowercase letters, digits and dashes, so "<op>@<step>"
// is an annotation value. Only StepReset means something to the reconciler.
type Step string

// StepReset is the rebuild step that starts the seed of the next root
// generation with --reset: from there on the previous root may be in trash
// (root.g<N>.trash), and undoing the rebuild needs the rollback tool Job.
const StepReset Step = "reset"

var stepGrammar = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Valid reports whether s may be journaled.
func (s Step) Valid() bool { return stepGrammar.MatchString(string(s)) }

// The object kinds a journal names, as `fleet cluster gc` lists them.
const (
	KindStatefulSet    = "sts"
	KindDeployment     = "deploy"
	KindJob            = "job"
	KindPod            = "pod"
	KindPVC            = "pvc"
	KindSecret         = "secret"
	KindConfigMap      = "configmap"
	KindService        = "svc"
	KindNetworkPolicy  = "netpol"
	KindPDB            = "pdb"
	KindVolumeSnapshot = "volumesnapshot"
)

// JournalObject is one object a job wrote.
type JournalObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// UID is the object's once the write that made or changed it
	// returned; "" while that write is only intended.
	UID string `json:"uid,omitempty"`
	// Gen is the root generation the object belongs to (0: none).
	Gen int `json:"gen,omitempty"`
	// Created: the job made the object, so undoing the job deletes it. An
	// object it only changed (a rebuild's StatefulSet) is put back instead.
	Created bool `json:"created,omitempty"`
	// Anchor: the job mirrors its step on this object (fleet.dev/op).
	Anchor bool `json:"anchor,omitempty"`
}

// Rollback is what undoing a rebuild past StepReset needs, recorded before
// the reset: the previous generation's (Gen-1) StatefulSet as fleet applied
// it, re-applied once the rollback tool Job (Job, a role=tool Job rendered
// for the instance's disk at podspec.JobDiskPath) has put that generation's
// root back. Both are server-side apply bodies (podspec.ApplyBody).
type Rollback struct {
	StatefulSet json.RawMessage `json:"statefulSet,omitempty"`
	Job         json.RawMessage `json:"job,omitempty"`
}

// Journal is one job's record.
type Journal struct {
	V        int    `json:"v"`
	Op       Op     `json:"op"`
	Fleet    string `json:"fleet"`
	Instance string `json:"inst"`
	// Cluster is the connection (KubeCluster.ID), Namespace its namespace
	// when the job started, Key the instance's key.
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key"`
	// Gen is the root generation the job makes (a rebuild's N+1).
	Gen int `json:"gen"`
	// Step is the step the job is in, PrevStep the one before: a step's
	// mirror on the anchor is written after its journal record, so the live
	// annotation may still name PrevStep.
	Step     Step `json:"step"`
	PrevStep Step `json:"prevStep,omitempty"`
	// ResetStarted: a rebuild reached StepReset, so its previous root may
	// be in trash.
	ResetStarted bool            `json:"resetStarted,omitempty"`
	Objects      []JournalObject `json:"objects"`
	// PrevReplicas, PrevSpecSecret and PrevStatus are what a rebuild found:
	// the StatefulSet's replicas, generation Gen-1's spec Secret (kept until
	// the rebuild succeeds) and the record's status and error, which a
	// rebuild undone before its reset returns to.
	PrevReplicas   *int32               `json:"prevReplicas,omitempty"`
	PrevSpecSecret string               `json:"prevSpecSecret,omitempty"`
	PrevStatus     fleet.InstanceStatus `json:"prevStatus,omitempty"`
	PrevError      string               `json:"prevError,omitempty"`
	Rollback       *Rollback            `json:"rollback,omitempty"`
	// Source is a clone's source instance, which the clone leaves as it found
	// it: one it stopped for its final copy (clone_consistency stop-source)
	// is started again when the clone is undone.
	Source    *JournalSource `json:"source,omitempty"`
	StartedAt time.Time      `json:"startedAt"`
}

// JournalSource is a clone's source.
type JournalSource struct {
	// StatefulSet and Key name the source instance (its key labels its
	// objects); UID is its StatefulSet's.
	StatefulSet string `json:"statefulSet"`
	Key         string `json:"key"`
	UID         string `json:"uid,omitempty"`
	// Stopped: the clone scaled the source to 0 (its replicas were
	// PrevReplicas); set before the scale, cleared once the source is
	// started again.
	Stopped      bool   `json:"stopped,omitempty"`
	PrevReplicas *int32 `json:"prevReplicas,omitempty"`
}

// Mark is the journal's mirror: the fleet.dev/op value its anchor carries.
func (j Journal) Mark() string { return MarkFor(j.Op, j.Step) }

// MarkFor is "<op>@<step>".
func MarkFor(op Op, step Step) string { return string(op) + "@" + string(step) }

// Current reports whether mark, an anchor's live fleet.dev/op, is this
// journal's: its step or the one before.
func (j Journal) Current(mark string) bool {
	return mark != "" && (mark == j.Mark() || (j.PrevStep != "" && mark == MarkFor(j.Op, j.PrevStep)))
}

// Anchor is the object the job mirrors its step on.
func (j Journal) Anchor() (JournalObject, bool) {
	for _, o := range j.Objects {
		if o.Anchor {
			return o, true
		}
	}
	return JournalObject{}, false
}

// Validate checks the journal is one this fleet can act on.
func (j Journal) Validate() error {
	if j.V > JournalVersion {
		return fmt.Errorf("%w: version %d, this fleet reads up to %d", ErrJournalNewer, j.V, JournalVersion)
	}
	if !j.Op.Valid() {
		return fmt.Errorf("unknown job op %q", j.Op)
	}
	if !names.ValidHash(j.Key) {
		return fmt.Errorf("instance key %q is not one", j.Key)
	}
	if j.Cluster == "" || j.Fleet == "" || j.Instance == "" {
		return errors.New("the journal names no connection, fleet or instance")
	}
	if !j.Step.Valid() || (j.PrevStep != "" && !j.PrevStep.Valid()) {
		return fmt.Errorf("step %q is not a step", j.Step)
	}
	for _, o := range j.Objects {
		if o.Kind == "" || o.Name == "" {
			return errors.New("the journal names an object without a kind or name")
		}
	}
	return nil
}

// ErrJournalNewer is a journal a newer fleet wrote: read-only to this one.
var ErrJournalNewer = errors.New("the journal was written by a newer fleet")

// ErrJournalExists is a job started on an instance whose previous job's
// journal is still there: that one has to be reconciled first.
var ErrJournalExists = errors.New("the instance already has a job journal")

// JournalPath is the journal of the instance with key.
func JournalPath(key string) string { return state.KubeJobPath(key) }

// JobJournal is a running job's journal. Every change is on disk before the
// method returns. Safe for concurrent use.
type JobJournal struct {
	mu   sync.Mutex
	path string
	j    Journal
	done bool
}

// BeginJournal records a job's start, before its first API server write.
// The journal goes at JournalPath(j.Key); a journal already there is
// ErrJournalExists.
func BeginJournal(j Journal) (*JobJournal, error) {
	return BeginJournalAt(JournalPath(j.Key), j)
}

// BeginJournalAt is BeginJournal at path.
func BeginJournalAt(path string, j Journal) (*JobJournal, error) {
	j.V = JournalVersion
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now().UTC()
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("job journal: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%w (%s)", ErrJournalExists, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("job journal: %w", err)
	}
	jj := &JobJournal{path: path, j: j}
	if err := jj.writeLocked(); err != nil {
		return nil, err
	}
	return jj, nil
}

// Path is where the journal is.
func (jj *JobJournal) Path() string { return jj.path }

// Journal is a copy of what the journal holds.
func (jj *JobJournal) Journal() Journal {
	jj.mu.Lock()
	defer jj.mu.Unlock()
	return jj.j.clone()
}

// Mark is the fleet.dev/op value the job's objects carry now.
func (jj *JobJournal) Mark() string {
	jj.mu.Lock()
	defer jj.mu.Unlock()
	return jj.j.Mark()
}

// Step records that the job moved to step. The caller then mirrors it on
// the anchor (Scope.MirrorStep).
func (jj *JobJournal) Step(step Step) error {
	if !step.Valid() {
		return fmt.Errorf("step %q is not a step", step)
	}
	return jj.Update(func(j *Journal) {
		if j.Step == step {
			return
		}
		if step == StepReset && j.Op == OpRebuild {
			j.ResetStarted = true
		}
		j.PrevStep, j.Step = j.Step, step
	})
}

// Intend records an object the job is about to write (UID still empty): a
// write the daemon died in is then still known.
func (jj *JobJournal) Intend(o JournalObject) error {
	if o.Kind == "" || o.Name == "" {
		return errors.New("journal: an object needs a kind and a name")
	}
	return jj.Update(func(j *Journal) {
		for i := range j.Objects {
			if j.Objects[i].Kind == o.Kind && j.Objects[i].Name == o.Name {
				uid := j.Objects[i].UID
				j.Objects[i] = o
				if o.UID == "" {
					j.Objects[i].UID = uid
				}
				return
			}
		}
		j.Objects = append(j.Objects, o)
	})
}

// Applied records the UID of an object the job wrote.
func (jj *JobJournal) Applied(kind, name, uid string) error {
	return jj.Update(func(j *Journal) {
		for i := range j.Objects {
			if j.Objects[i].Kind == kind && j.Objects[i].Name == name {
				j.Objects[i].UID = uid
				return
			}
		}
		j.Objects = append(j.Objects, JournalObject{Kind: kind, Name: name, UID: uid})
	})
}

// Update changes the journal with fn (a rebuild's Prev* and Rollback) and
// writes it.
func (jj *JobJournal) Update(fn func(*Journal)) error {
	jj.mu.Lock()
	defer jj.mu.Unlock()
	if jj.done {
		return errors.New("journal: the job already finished")
	}
	next := jj.j.clone()
	fn(&next)
	next.V = JournalVersion
	if err := next.Validate(); err != nil {
		return err
	}
	prev := jj.j
	jj.j = next
	if err := jj.writeLocked(); err != nil {
		jj.j = prev
		return err
	}
	return nil
}

// Finish removes the journal: the job ended and needs no reconciling. The
// caller first clears the mirror (Scope.ClearMark).
func (jj *JobJournal) Finish() error {
	jj.mu.Lock()
	defer jj.mu.Unlock()
	jj.done = true
	return RemoveJournal(jj.path)
}

func (jj *JobJournal) writeLocked() error {
	data, err := json.MarshalIndent(jj.j, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(jj.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("job journal: %w", err)
	}
	return nil
}

func (j Journal) clone() Journal {
	c := j
	c.Objects = append([]JournalObject(nil), j.Objects...)
	if j.PrevReplicas != nil {
		n := *j.PrevReplicas
		c.PrevReplicas = &n
	}
	if j.Rollback != nil {
		rb := *j.Rollback
		c.Rollback = &rb
	}
	if j.Source != nil {
		src := *j.Source
		if j.Source.PrevReplicas != nil {
			n := *j.Source.PrevReplicas
			src.PrevReplicas = &n
		}
		c.Source = &src
	}
	return c
}

// ReadJournal reads the journal at path. A journal a newer fleet wrote comes
// back with ErrJournalNewer; one that does not parse or validate, with why.
func ReadJournal(path string) (Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Journal{}, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return Journal{}, fmt.Errorf("job journal %s: %w", filepath.Base(path), err)
	}
	if j.V > JournalVersion {
		return j, fmt.Errorf("job journal %s: %w (version %d)", filepath.Base(path), ErrJournalNewer, j.V)
	}
	if err := j.Validate(); err != nil {
		return j, fmt.Errorf("job journal %s: %w", filepath.Base(path), err)
	}
	return j, nil
}

// JournalFile is one journal in the jobs directory.
type JournalFile struct {
	Path    string
	Journal Journal
	// Err is why the file could not be used (ErrJournalNewer among them);
	// the reconciler leaves such a file alone.
	Err error
}

// ListJournals reads every journal in dir (state.KubeJobsDir), oldest job
// first. A missing dir has none.
func ListJournals(dir string) ([]JournalFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []JournalFile
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		j, err := ReadJournal(path)
		if err == nil && strings.TrimSuffix(name, ".json") != j.Key {
			err = fmt.Errorf("job journal %s names key %s", name, j.Key)
		}
		out = append(out, JournalFile{Path: path, Journal: j, Err: err})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Journal.StartedAt.Before(out[b].Journal.StartedAt) })
	return out, nil
}

// RemoveJournal deletes the journal at path; a missing one is fine.
func RemoveJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("job journal: %w", err)
	}
	return nil
}
