package sharesync

import (
	"cmp"
	"context"
	"fmt"
)

// Mirror is the mirror policy: the newest change to a path wins everywhere,
// deletes included. Concurrent changes (one made without seeing the other)
// are decided by modification time, and the loser is kept as a conflict
// copy; a delete never wins over a change it did not see, so a concurrent
// edit is never lost to it.
type Mirror struct{}

// Scope is fleet-wide.
func (Mirror) Scope() Scope { return ScopeFleet }

// Lane leaves the lane to the size.
func (Mirror) Lane() Lane { return LaneAuto }

// History keeps no earlier contents.
func (Mirror) History() int { return 0 }

// Merge decides by newest change.
func (Mirror) Merge(_ context.Context, in *MergeInput) (MergeResult, error) {
	return newestWins(in), nil
}

// newestWins is mirror's decision, which other policies fall back to for
// content they cannot merge.
func newestWins(in *MergeInput) MergeResult {
	if in.Fast || in.Current == nil {
		return MergeResult{Decision: Accept}
	}
	cur, inc := in.Current.Entry, in.Incoming.Entry
	switch {
	case !inc.Live():
		return MergeResult{Decision: Keep, Reason: fmt.Sprintf("a delete from %s that had not seen a newer change", in.Side.ID)}
	case !cur.Live():
		return MergeResult{Decision: Accept, Reason: fmt.Sprintf("a change from %s made after a delete it had not seen", in.Side.ID)}
	case newer(inc, cur):
		return MergeResult{Decision: Accept, Conflict: true, Reason: fmt.Sprintf("a newer change from %s", in.Side.ID)}
	}
	return MergeResult{Decision: Keep, Conflict: true, Reason: fmt.Sprintf("an older change from %s", in.Side.ID)}
}

// newer orders two live entries for mirror, by modification time and then by
// content, so every copy deciding the same pair agrees whichever arrived
// first.
func newer(a, b Entry) bool {
	if c := cmp.Compare(a.MTime, b.MTime); c != 0 {
		return c > 0
	}
	if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
		return c > 0
	}
	if c := cmp.Compare(a.Hash, b.Hash); c != 0 {
		return c > 0
	}
	if c := cmp.Compare(a.Target, b.Target); c != 0 {
		return c > 0
	}
	return a.Mode > b.Mode
}
