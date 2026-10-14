package sharesync

import (
	"fmt"
	"slices"
	"strings"
)

// Status is a snapshot of the hub, for the instance rows, `fleet sync
// status` and the fleet options.
type Status struct {
	Roots []RootStatus
	// HostPaused says why decisions that keep a conflict copy wait (the
	// daemon host's disk is too full for the copy).
	HostPaused string
}

// RootStatus is one root's state.
type RootStatus struct {
	Root string
	Rev  uint64
	// Files and Bytes count the root's live files (every location's).
	Files int
	Bytes int64
	// Cap is the root's size cap, 0 for none.
	Cap int64
	// Paused says why changes made in the root wait (it is over its cap).
	Paused string
	// Warnings flag parts of the root grown large.
	Warnings []string
	// Broken says why the root is not synced at all.
	Broken string
	Sides  []SideStatus
}

// SideStatus is one side's state in one root.
type SideStatus struct {
	Side, Location string
	// Pending counts the paths still to send the side, PendingBytes their
	// files' bytes.
	Pending      int
	PendingBytes int64
	// Paused says why the side cannot take files now ("share full").
	Paused string
	// Refused lists paths the side refused, with why.
	Refused []string
	// LastError is the side's last failure.
	LastError string
}

// PauseReasons returns each distinct reason sync is paused anywhere, as the
// `sync paused: <reason>` badge words it.
func (s Status) PauseReasons() []string {
	var out []string
	add := func(reason string) {
		if reason != "" && !slices.Contains(out, reason) {
			out = append(out, reason)
		}
	}
	add(s.HostPaused)
	for _, r := range s.Roots {
		add(r.Paused)
		add(r.Broken)
		for _, side := range r.Sides {
			add(side.Paused)
		}
	}
	return out
}

// Status returns a snapshot of the hub.
func (h *Hub) Status() Status {
	var st Status
	if reason := h.hostFull.Load(); reason != nil {
		st.HostPaused = *reason
	}
	for _, root := range h.order {
		root.mu.Lock()
		rs := RootStatus{Root: root.name, Rev: root.rev, Bytes: root.total, Cap: root.cap}
		if root.broken != nil && root.broken != ErrClosed {
			rs.Broken = root.broken.Error()
		}
		if len(root.holds) > 0 {
			rs.Paused = root.capReason()
		}
		warned := make([]int64, len(root.warnings))
		for k, e := range root.entries {
			if e.Entry.Kind != File {
				continue
			}
			rs.Files++
			for i, w := range root.warnings {
				if w.Matches(k.path) {
					warned[i] += e.Entry.Size
				}
			}
		}
		for i, w := range root.warnings {
			if warned[i] > w.Size {
				rs.Warnings = append(rs.Warnings, fmt.Sprintf("%s/%s holds %s (over %s)", root.name, w.Pattern, sizeString(warned[i]), sizeString(w.Size)))
			}
		}
		for id, sr := range root.sides {
			ss := SideStatus{Side: id, Location: sr.side.info.Location, Paused: sr.paused, LastError: sr.lastErr}
			for p := range sr.dirty {
				if op, ok := root.opFor(sr, p); ok {
					if r, blocked := sr.blocked[p]; blocked && r.rev == op.Rev {
						continue
					}
					ss.Pending++
					if op.Entry.Kind == File {
						ss.PendingBytes += op.Entry.Size
					}
				}
			}
			ss.Pending += len(sr.inflight)
			for p, r := range sr.blocked {
				ss.Refused = append(ss.Refused, p+": "+r.reason)
			}
			slices.Sort(ss.Refused)
			rs.Sides = append(rs.Sides, ss)
		}
		root.mu.Unlock()
		slices.SortFunc(rs.Sides, func(a, b SideStatus) int { return strings.Compare(a.Side, b.Side) })
		st.Roots = append(st.Roots, rs)
	}
	return st
}
