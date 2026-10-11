package sharesync

import (
	"context"
	"errors"
	"fmt"
)

// mergeHistory is how many earlier contents of a settings file the hub keeps
// for its three-way merges: a change made a few revs back (a side that was
// behind when it wrote) still finds its base.
const mergeHistory = 4

// docFormat is a settings file format the three-way policies merge.
type docFormat interface {
	// name names the format in reasons.
	name() string
	// parse reads a whole document; an error means data is not one (a
	// write under way, or a file saved mid-edit).
	parse(data []byte) (*value, error)
	// seed reports whether v is the empty document a new instance seeds,
	// which never wins over content.
	seed(v *value) bool
	// render writes merged in the format of like (the current copy's
	// content; cur is its parse).
	render(merged *value, like []byte, cur *value) ([]byte, error)
}

// mergeDocument is json3 and toml3's decision: a whole document of the
// format is taken as it is when nothing else changed the path since its
// base; two changed copies merge three ways; the seed never replaces
// content, and a file that does not parse waits on its side (ErrIncomplete)
// until a whole one is written. Deletes, other kinds and files too large to
// merge go as mirror.
func mergeDocument(ctx context.Context, in *MergeInput, f docFormat) (MergeResult, error) {
	if in.Incoming.Entry.Kind != File {
		return newestWins(in), nil
	}
	data, err := in.Incoming.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		return newestWins(in), nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	inc, err := f.parse(data)
	if err != nil {
		return MergeResult{}, fmt.Errorf("%s from %s: %w (%v)", in.Path, in.Side.ID, ErrIncomplete, err)
	}
	current := in.Current != nil && in.Current.Entry.Kind == File
	if current && f.seed(inc) {
		// The seed of a new instance (or of a share being laid out) meeting
		// the fleet's file: never written over it, wherever it came from.
		return MergeResult{Decision: Keep, Reason: fmt.Sprintf("an empty %s from %s never replaces content", f.name(), in.Side.ID)}, nil
	}
	if in.Fast || !current {
		return newestWins(in), nil
	}
	curData, err := in.Current.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		return newestWins(in), nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	cur, err := f.parse(curData)
	if err != nil || f.seed(cur) {
		return MergeResult{Decision: Accept, Reason: fmt.Sprintf("a whole %s from %s replaces an empty or unreadable one", f.name(), in.Side.ID)}, nil
	}
	var base *value
	if b, ok, err := in.Base(ctx); err != nil {
		return MergeResult{}, err
	} else if ok {
		// No base (too old, or the change was made on nothing) merges two
		// ways: whatever either copy has is kept.
		base, _ = f.parse(b)
	}
	m := &merger3{incomingWins: newer(in.Incoming.Entry, in.Current.Entry)}
	merged := m.merge(base, cur, inc, "")
	reason := fmt.Sprintf("%s changes from %s merged%s", f.name(), in.Side.ID, conflictNote(m.conflicts))
	switch {
	case same(merged, cur):
		return MergeResult{Decision: Keep, Reason: reason}, nil
	case same(merged, inc):
		return MergeResult{Decision: Accept, Reason: reason}, nil
	case f.seed(merged):
		// Each copy removed what the other kept: the merge would leave the
		// seed, which never replaces content.
		res := newestWins(in)
		res.Reason = fmt.Sprintf("%s changes from %s would merge to an empty %s; the newer copy kept", f.name(), in.Side.ID, f.name())
		return res, nil
	}
	content, err := f.render(merged, curData, cur)
	if err != nil {
		res := newestWins(in)
		res.Reason = fmt.Sprintf("%s changes from %s could not be written merged (%v); the newer copy kept", f.name(), in.Side.ID, err)
		return res, nil
	}
	return MergeResult{Decision: Merged, Content: content, Reason: reason}, nil
}
