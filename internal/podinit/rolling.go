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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &RollingLog{dir: dir, segMax: max / 2}
	r.state = readRollingState(dir)
	f, err := os.OpenFile(filepath.Join(dir, RollingName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
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
	st := rollingState{V: rollingVersion}
	data, err := os.ReadFile(filepath.Join(dir, rollingIndex))
	if err != nil {
		return st
	}
	var got rollingState
	if json.Unmarshal(data, &got) != nil || got.V != rollingVersion || got.Base < got.PrevBase {
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
	f, err := os.OpenFile(cur, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o644)
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
	// Data is the run's output; Truncated when its start rotated away.
	Data      []byte
	Truncated bool
	// End is the offset just past Data.
	End int64
}

// loadRolling reads the segments: everything still held, and the offsets of
// its first byte and just past its last.
func loadRolling(dir string) (st rollingState, all []byte, start, end int64, err error) {
	st = readRollingState(dir)
	prev, err := os.ReadFile(filepath.Join(dir, rollingPrev))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, nil, 0, 0, err
	}
	cur, err := os.ReadFile(filepath.Join(dir, RollingName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, nil, 0, 0, err
	}
	// After a crash mid-rotate the index may disagree with the files: trust
	// the files.
	st.PrevBase = st.Base - int64(len(prev))
	all = append(prev, cur...)
	return st, all, st.PrevBase, st.Base + int64(len(cur)), nil
}

// ReadRolling reads what the rolling file in dir holds, run by run, oldest
// first. fleetd reads it back on the next start to save what a lost node's
// container log never delivered: the runs it has no log of, or the bytes past
// the offset it saved up to (ReadRollingSince).
func ReadRolling(dir string) ([]RunOutput, error) {
	st, all, start, end, err := loadRolling(dir)
	if err != nil {
		return nil, err
	}
	if len(st.Runs) == 0 {
		if len(all) == 0 {
			return nil, nil
		}
		return []RunOutput{{RunMark: RunMark{Offset: start}, Data: all, End: end}}, nil
	}
	var out []RunOutput
	for i, run := range st.Runs {
		to := end
		if i+1 < len(st.Runs) {
			to = min(st.Runs[i+1].Offset, end)
		}
		from := max(run.Offset, start)
		if to <= start || from > to {
			continue
		}
		out = append(out, RunOutput{RunMark: run, Data: all[from-start : to-start], Truncated: run.Offset < start, End: to})
	}
	return out, nil
}

// ReadRollingSince is the rolling file's bytes from offset on, the offset just
// past them, and how many bytes from offset on had already rotated away.
func ReadRollingSince(dir string, offset int64) (data []byte, next int64, lost int64, err error) {
	_, all, start, end, err := loadRolling(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	if offset < start {
		lost, offset = start-offset, start
	}
	if offset >= end {
		return nil, end, lost, nil
	}
	return all[offset-start:], end, lost, nil
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
