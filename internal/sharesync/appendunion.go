package sharesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// AppendUnion is the append-union policy, for append-only logs
// (history.jsonl): concurrent copies converge on the union of their lines,
// so a line appended anywhere is never lost. A change made on the current
// state is taken as it is (a deliberate truncation included); a delete that
// had not seen newer lines is refused. Content that is not a file, or too
// large to merge, falls back to mirror.
type AppendUnion struct{}

// Scope is fleet-wide.
func (AppendUnion) Scope() Scope { return ScopeFleet }

// Lane leaves the lane to the size.
func (AppendUnion) Lane() Lane { return LaneAuto }

// History keeps no earlier contents: the union needs no base.
func (AppendUnion) History() int { return 0 }

// Merge unions concurrent copies.
func (AppendUnion) Merge(ctx context.Context, in *MergeInput) (MergeResult, error) {
	if in.Fast || in.Current == nil {
		return MergeResult{Decision: Accept}, nil
	}
	cur, inc := in.Current.Entry, in.Incoming.Entry
	if cur.Kind != File || inc.Kind != File {
		return newestWins(in), nil
	}
	current, err := in.Current.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		return newestWins(in), nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	incoming, err := in.Incoming.Content(ctx)
	if errors.Is(err, ErrTooLarge) {
		return newestWins(in), nil
	}
	if err != nil {
		return MergeResult{}, err
	}
	merged := unionLines(current, incoming)
	reason := fmt.Sprintf("lines from %s merged", in.Side.ID)
	switch {
	case bytes.Equal(merged, current):
		return MergeResult{Decision: Keep, Reason: reason}, nil
	case bytes.Equal(merged, incoming):
		return MergeResult{Decision: Accept, Reason: reason}, nil
	}
	return MergeResult{Decision: Merged, Content: merged, Reason: reason}, nil
}

// unionLines returns a followed by the lines of b that a lacks, counted as a
// multiset (a line b has twice and a once is added once). A last line
// without its newline is a line still being written: when the other copy
// holds it completed (a line starting with it), only the completed one is
// kept.
func unionLines(a, b []byte) []byte {
	la, aOpen := splitLines(a)
	lb, bOpen := splitLines(b)
	dropped := false
	if aOpen && completedBy(la[len(la)-1], lb, bOpen) {
		la, aOpen, dropped = la[:len(la)-1], false, true
	}
	if bOpen && completedBy(lb[len(lb)-1], la, aOpen) {
		lb, bOpen = lb[:len(lb)-1], false
	}
	have := map[string]int{}
	for _, line := range la {
		have[line]++
	}
	var extra []string
	extraOpen := false
	for i, line := range lb {
		if have[line] > 0 {
			have[line]--
			continue
		}
		extra = append(extra, line)
		extraOpen = bOpen && i == len(lb)-1
	}
	if len(extra) == 0 && !dropped {
		return a
	}
	lines := append(la, extra...)
	open := extraOpen || aOpen && len(extra) == 0
	var out bytes.Buffer
	for i, line := range lines {
		out.WriteString(line)
		if i < len(lines)-1 || !open {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// splitLines splits data into its lines, without their newlines; open is set
// when the last one has none.
func splitLines(data []byte) (lines []string, open bool) {
	if len(data) == 0 {
		return nil, false
	}
	open = data[len(data)-1] != '\n'
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		lines = append(lines, string(line))
	}
	return lines, open
}

// completedBy reports whether a line still being written, tail, is held
// completed in others: a complete line starting with it, or a longer line
// still being written (others' last one, when othersOpen).
func completedBy(tail string, others []string, othersOpen bool) bool {
	for i, line := range others {
		if !strings.HasPrefix(line, tail) {
			continue
		}
		if len(line) > len(tail) || !(othersOpen && i == len(others)-1) {
			return true
		}
	}
	return false
}
