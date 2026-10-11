package sharesync

import (
	"context"
	"fmt"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
)

// Scope says which copies of a path one state is kept for.
type Scope uint8

const (
	// ScopeFleet: one state for every copy in the fleet.
	ScopeFleet Scope = iota
	// ScopeLocation: one state per location (the daemon host, each cluster),
	// kept live among that location's copies and never crossing to another.
	ScopeLocation
)

// Merger implements one sync policy: what becomes of each change the hub
// receives for a path of that policy. The hub calls Merge for every change,
// fast forwards included, outside its locks; the content accessors fetch on
// demand.
type Merger interface {
	// Scope is the policy's scope.
	Scope() Scope
	// Lane is the lane the policy's changes travel on, or LaneAuto.
	Lane() Lane
	// History is how many of a path's earlier contents the hub keeps for
	// MergeInput.Base, or 0. Only contents up to MaxMergeSize are kept.
	History() int
	// Merge decides one reported change.
	Merge(ctx context.Context, in *MergeInput) (MergeResult, error)
}

// MaxMergeSize bounds the content a merge reads into memory.
const MaxMergeSize = 16 << 20

// MergeInput is one reported change and the state it meets.
type MergeInput struct {
	// Root and Path name the path, Path relative to Root.
	Root, Path string
	// Side is the side that reported the change.
	Side SideInfo
	// Fast is set when the change was made on the path's current state:
	// nothing else changed the path since the side last synced it.
	Fast bool
	// Current is the path's current state, or nil when the hub has never
	// had the path (or has forgotten an old delete of it).
	Current *Version
	// Incoming is the reported change.
	Incoming Version

	base func(ctx context.Context) ([]byte, bool, error)
}

// Base returns the content the change was made on (the path's content at
// the change's base rev), when the policy keeps History and the hub still
// has it; ok is false otherwise.
func (in *MergeInput) Base(ctx context.Context) (content []byte, ok bool, err error) {
	if in.base == nil {
		return nil, false, nil
	}
	return in.base(ctx)
}

// Version is one state of a path.
type Version struct {
	Entry Entry
	// Rev is the state's rev; 0 for an incoming change.
	Rev uint64
	// Side is the side the content came from, or "" for content the hub
	// made (a merge).
	Side string

	open func(ctx context.Context) (io.ReadCloser, error)
}

// Content reads a file version's content: ErrTooLarge over MaxMergeSize,
// nil for anything but a file.
func (v *Version) Content(ctx context.Context) ([]byte, error) {
	if v.Entry.Kind != File {
		return nil, nil
	}
	if v.Entry.Size > MaxMergeSize {
		return nil, ErrTooLarge
	}
	if v.open == nil {
		return nil, ErrNoSource
	}
	rc, err := v.open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, MaxMergeSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != v.Entry.Size {
		return nil, ErrStale
	}
	return data, nil
}

// Decision is what a merge makes of a change.
type Decision uint8

const (
	// Accept: the incoming change becomes the path's state.
	Accept Decision = iota + 1
	// Keep: the current state stays; the reporting side is given it.
	Keep
	// Merged: MergeResult.Content becomes the path's state everywhere,
	// the reporting side included.
	Merged
)

// MergeResult is a merge's decision.
type MergeResult struct {
	Decision Decision
	// Content is the merged file, for Merged; it takes the incoming
	// change's mode.
	Content []byte
	// Conflict keeps the losing version (Current on Accept, Incoming on
	// Keep) as a conflict copy, when it is a file or a symlink.
	Conflict bool
	// Reason says why, for the log and the conflict list.
	Reason string
}

// Policies maps each sync policy to its implementation. LocationLocal needs
// none: its paths are never scanned or sent.
type Policies map[syncpolicy.Policy]Merger

// DefaultPolicies returns the policies this package implements: mirror and
// append-union. Credential, JSON3 and TOML3 come from their own Mergers.
func DefaultPolicies() Policies {
	return Policies{
		syncpolicy.Mirror:      Mirror{},
		syncpolicy.AppendUnion: AppendUnion{},
	}
}

// covers checks that every policy a root of the table can give a path in
// name has an implementation.
func (p Policies) covers(table syncpolicy.Table, name string) error {
	for _, root := range table.Roots() {
		if !within(root.Path, name) && !within(name, root.Path) {
			continue
		}
		policies := []syncpolicy.Policy{root.Default}
		for _, rule := range root.Rules {
			policies = append(policies, rule.Policy)
		}
		for _, policy := range policies {
			if policy == syncpolicy.LocationLocal {
				continue
			}
			if p[policy] == nil {
				return fmt.Errorf("sharesync: root %s: no implementation of the %s policy (root %s)", name, policy, root.Path)
			}
		}
	}
	return nil
}
