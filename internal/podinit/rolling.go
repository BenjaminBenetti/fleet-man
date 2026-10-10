package podinit

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The rolling copy of PID 1's output lives in two segments, the current one
// and the one before it, each at most half the cap, plus a small index. An
// offset is a byte's position in everything ever written, so a reader that
// saved up to offset N knows exactly what it has not seen.
const (
	// RollingName is the current segment.
	RollingName = "pid1.log"
	// rollingPrev is the previous segment.
	rollingPrev = RollingName + ".1"
	// rollingIndex records the segments' offsets and the runs.
	rollingIndex   = RollingName + ".json"
	rollingVersion = 1
	// maxRuns caps the runs the index remembers.
	maxRuns = 64
)

// RunMark marks where one start of the container begins in the output.
type RunMark struct {
	// ID is pod-init's own random id for the run.
	ID string `json:"id"`
	// PodUID is the pod's uid (FLEET_POD_UID, from the downward API), when
	// the renderer passed it.
	PodUID string `json:"pod_uid,omitempty"`
	// Start is when pod-init started.
	Start time.Time `json:"start"`
	// Offset is where the run's output begins.
	Offset int64 `json:"offset"`
}

type rollingState struct {
	V int `json:"v"`
	// Base is the offset of the current segment's first byte, PrevBase the
	// previous segment's.
	Base     int64     `json:"base"`
	PrevBase int64     `json:"prev_base"`
	Runs     []RunMark `json:"runs,omitempty"`
}

// RollingLog appends to the rolling file. Writes never fail the caller: a
// full or broken disk turns the copy off for a while, never PID 1's output.
type RollingLog struct {
	dir    string
	segMax int64

	mu       sync.Mutex
	f        *os.File
	size     int64
	state    rollingState
	disabled time.Time
}

// OpenRolling opens the rolling file in dir (max total bytes, 0 for
// DefaultLogMax) and records run's start at the current end.
func OpenRolling(dir string, max int64, run RunMark) (*RollingLog, error) {
	if max <= 0 {
		max = DefaultLogMax
	}
	// Only root (and fleetd, through the agent) reads PID 1's output, as only
	// the docker host reads a container's log.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &RollingLog{dir: dir, segMax: max / 2}
	r.state = readRollingState(dir)
	f, err := os.OpenFile(filepath.Join(dir, RollingName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r.f, r.size = f, fi.Size()
	run.Offset = r.state.Base + r.size
	r.state.Runs = append(r.state.Runs, run)
	if len(r.state.Runs) > maxRuns {
		r.state.Runs = r.state.Runs[len(r.state.Runs)-maxRuns:]
	}
	if err := r.saveState(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func readRollingState(dir string) rollingState {
	data, _ := os.ReadFile(filepath.Join(dir, rollingIndex))
	return parseRollingState(data)
}

func parseRollingState(data []byte) rollingState {
	st := rollingState{V: rollingVersion}
	var got rollingState
	if len(data) == 0 || json.Unmarshal(data, &got) != nil || got.V != rollingVersion || got.Base < got.PrevBase {
		return st
	}
	return got
}

func (r *RollingLog) saveState() error {
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.dir, "."+rollingIndex+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, rollingIndex))
}

// Write appends p, rotating the segments when the current one is full.
func (r *RollingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil || time.Now().Before(r.disabled) {
		return len(p), nil
	}
	if r.size > 0 && r.size+int64(len(p)) > r.segMax {
		if err := r.rotate(); err != nil {
			r.disabled = time.Now().Add(30 * time.Second)
			return len(p), nil
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	if err != nil {
		r.disabled = time.Now().Add(30 * time.Second)
	}
	return len(p), nil
}

// rotate makes the current segment the previous one and starts a new one.
func (r *RollingLog) rotate() error {
	cur := filepath.Join(r.dir, RollingName)
	if err := os.Rename(cur, filepath.Join(r.dir, rollingPrev)); err != nil {
		return err
	}
	_ = r.f.Close()
	r.state.PrevBase = r.state.Base
	r.state.Base += r.size
	var runs []RunMark
	for i, run := range r.state.Runs {
		// Keep the runs still in a segment, and the one the previous segment
		// starts inside.
		if run.Offset >= r.state.PrevBase || i+1 == len(r.state.Runs) || r.state.Runs[i+1].Offset > r.state.PrevBase {
			runs = append(runs, run)
		}
	}
	r.state.Runs = runs
	f, err := os.OpenFile(cur, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		r.f = nil
		return err
	}
	r.f, r.size = f, 0
	return r.saveState()
}

// Close closes the file.
func (r *RollingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// RunOutput is one run's output as the rolling file still holds it.
type RunOutput struct {
	RunMark
	// PodSeq is the run's place among the runs of its pod the index still
	// records, from 0: the pod's n-th start of pod-init, which the kubelet
	// counts as its container's restart n (while no earlier run of the pod
	// has rotated out of the index; fleetd names a run by it).
	PodSeq int
	// Data is the run's output; Truncated when its start rotated away.
	Data      []byte
	Truncated bool
	// End is the offset just past Data.
	End int64
}

// RollingFiles are the rolling file's three parts, as fleetd fetches them
// from a pod (in RollingDirFiles order): the index, the previous segment and
// the current one. A missing part is nil.
type RollingFiles struct {
	Index, Prev, Cur []byte
}

// RollingDirFiles are the rolling file's parts, relative to its dir.
var RollingDirFiles = []string{rollingIndex, rollingPrev, RollingName}

// Rolling is a parsed rolling file: everything it still holds, with offsets.
type Rolling struct {
	runs []RunMark
	all  []byte
	// Start and End are the offsets of the first byte still held and just
	// past the last.
	Start, End int64
}

// ParseRolling parses the rolling file's parts.
func ParseRolling(f RollingFiles) Rolling {
	st := parseRollingState(f.Index)
	// After a crash mid-rotate the index may disagree with the segments:
	// trust the segments.
	start := st.Base - int64(len(f.Prev))
	all := append(append([]byte(nil), f.Prev...), f.Cur...)
	return Rolling{runs: st.Runs, all: all, Start: start, End: st.Base + int64(len(f.Cur))}
}

// LoadRolling reads and parses the rolling file in dir.
func LoadRolling(dir string) (Rolling, error) {
	var f RollingFiles
	for i, part := range []*[]byte{&f.Index, &f.Prev, &f.Cur} {
		data, err := os.ReadFile(filepath.Join(dir, RollingDirFiles[i]))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Rolling{}, err
		}
		*part = data
	}
	return ParseRolling(f), nil
}

// Runs is what the rolling file holds, run by run, oldest first. fleetd reads
// it back on the next start to save what a lost node's container log never
// delivered: the runs it has no log of, or the bytes past the offset it saved
// up to (Since).
func (r Rolling) Runs() []RunOutput {
	if len(r.runs) == 0 {
		if len(r.all) == 0 {
			return nil
		}
		return []RunOutput{{RunMark: RunMark{Offset: r.Start}, Data: r.all, End: r.End}}
	}
	var out []RunOutput
	seq := map[string]int{}
	for i, run := range r.runs {
		n := seq[run.PodUID]
		seq[run.PodUID] = n + 1
		to := r.End
		if i+1 < len(r.runs) {
			to = min(r.runs[i+1].Offset, r.End)
		}
		from := max(run.Offset, r.Start)
		if to <= r.Start || from > to {
			continue
		}
		out = append(out, RunOutput{RunMark: run, PodSeq: n, Data: r.all[from-r.Start : to-r.Start], Truncated: run.Offset < r.Start, End: to})
	}
	return out
}

// Since is the bytes from offset on, and how many bytes from offset on had
// already rotated away.
func (r Rolling) Since(offset int64) (data []byte, lost int64) {
	if offset < r.Start {
		lost, offset = r.Start-offset, r.Start
	}
	if offset >= r.End {
		return nil, lost
	}
	return r.all[offset-r.Start:], lost
}

// ReadRolling reads the runs of the rolling file in dir (LoadRolling, Runs).
func ReadRolling(dir string) ([]RunOutput, error) {
	r, err := LoadRolling(dir)
	if err != nil {
		return nil, err
	}
	return r.Runs(), nil
}

// ReadRollingSince is the rolling file's bytes from offset on, the offset just
// past them, and how many bytes from offset on had already rotated away.
func ReadRollingSince(dir string, offset int64) (data []byte, next int64, lost int64, err error) {
	r, err := LoadRolling(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	data, lost = r.Since(offset)
	return data, r.End, lost, nil
}

// teeWriter writes to w and the rolling log; the log never fails it.
type teeWriter struct {
	w   io.Writer
	log *RollingLog
}

func (t teeWriter) Write(p []byte) (int, error) {
	if t.log != nil {
		_, _ = t.log.Write(p)
	}
	n, err := t.w.Write(p)
	if err != nil {
		// The container log is gone (nothing reads it); the rolling copy
		// still has it.
		return len(p), nil
	}
	return n, nil
}
