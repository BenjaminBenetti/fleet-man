package sharesync

import (
	"context"
	"io"
)

// The messages between a side and the hub. They are plain data, so a spoke
// in a pod exchanges the same structs over its sessions (as JSON) that the
// daemon host's own Tree exchanges in process.

// SideInfo names a side.
type SideInfo struct {
	// ID is unique among the hub's sides ("host", or a cluster share or one
	// instance's copy).
	ID string `json:"id"`
	// Location is where the side's content lives: the daemon host, or one
	// cluster. A policy of location scope (logins) keeps one state per
	// location; every side of a location sees the same, and no other
	// location's.
	Location string `json:"location"`
	// LoginsOnly: the side keeps only the state of location scope (its
	// location's logins), as a location's login store on the daemon host
	// does. The hub sends it nothing else and takes nothing else from it.
	// fleetd's own setting, never on the wire.
	LoginsOnly bool `json:"-"`
}

// Change is one path a side found different from what it last synced.
type Change struct {
	Path string `json:"p"`
	// Base is the rev the side held for Path when it made the change, or 0
	// when it held none.
	Base  uint64 `json:"b,omitempty"`
	Entry Entry  `json:"e"`
}

// Report is a side's changes in one root, made in the hub's Epoch for that
// root (a side's revs mean nothing in another epoch).
type Report struct {
	Root    string   `json:"root"`
	Epoch   string   `json:"epoch,omitempty"`
	Changes []Change `json:"changes"`
}

// AckStatus is what the hub did with a reported change.
type AckStatus uint8

const (
	// Accepted: the change is the path's current state, at Ack.Rev. The
	// side now holds that rev.
	Accepted AckStatus = iota + 1
	// Superseded: the hub has the change and decided otherwise (a newer
	// change won, the copies merged, a stale delete was refused); an op
	// with the outcome follows. The side may now be overwritten there.
	// Ack.Rev is the rev of the state that stands, or 0 when the hub
	// refused a change made on its current state (a policy's guard: the
	// '{}' seed, a cleared login); the side then holds no rev of the
	// hub's there.
	Superseded
	// Deferred: the hub cannot take the change yet (the root is over its
	// cap, or a conflict copy could not be kept). The side keeps it and
	// reports it again.
	Deferred
	// Ignored: the change was malformed, or is for a path sharesync does
	// not sync.
	Ignored
)

// Ack answers one reported change.
type Ack struct {
	Path   string    `json:"p"`
	Rev    uint64    `json:"r,omitempty"`
	Status AckStatus `json:"s"`
	// Entry is the reported entry this answers, so a side that changed the
	// path again since can tell.
	Entry Entry `json:"e"`
}

// Reply answers a Report.
type Reply struct {
	Epoch string `json:"epoch"`
	Acks  []Ack  `json:"acks"`
}

// Held is a path a side holds at a rev, sent when it attaches.
type Held struct {
	Path  string `json:"p"`
	Rev   uint64 `json:"r"`
	Entry Entry  `json:"e"`
}

// RootHello is what a side holds of one root.
type RootHello struct {
	Root  string `json:"root"`
	Epoch string `json:"epoch,omitempty"`
	Held  []Held `json:"held"`
}

// Hello is what a side holds, sent when it attaches.
type Hello struct {
	Roots []RootHello `json:"roots"`
}

// Welcome is the hub's answer to a Hello: its epoch for each root.
type Welcome struct {
	Epochs map[string]string `json:"epochs"`
}

// Lane says how soon a change travels to a side and on which session.
type Lane uint8

const (
	// LaneAuto lets the size decide: bulk for files over BulkSize, normal
	// otherwise. A Merger returns it for no preference.
	LaneAuto Lane = iota
	// LanePriority goes at once (logins).
	LanePriority
	// LaneNormal goes after a 100ms quiet period.
	LaneNormal
	// LaneBulk goes after 1s, on the bulk session.
	LaneBulk
)

// String returns the lane's name.
func (l Lane) String() string {
	switch l {
	case LanePriority:
		return "priority"
	case LaneNormal:
		return "normal"
	case LaneBulk:
		return "bulk"
	}
	return "auto"
}

// BulkSize is the file size over which a change travels on the bulk lane.
const BulkSize = 1 << 20

// Op is one path the hub sends a side: the state it should hold at Rev. A
// Gone entry is a delete. A side never applies an op older than the rev it
// holds the path at.
type Op struct {
	Path  string `json:"p"`
	Rev   uint64 `json:"r"`
	Entry Entry  `json:"e"`
}

// ContentFunc opens the content of an op's file from offset on. A side that
// holds a prefix of the content (Entry.Grew) asks only for the rest.
type ContentFunc func(ctx context.Context, op Op, offset int64) (io.ReadCloser, error)

// Batch is ops for one root, applied in order: deletes deepest first, then
// everything else parents first.
type Batch struct {
	Root  string `json:"root"`
	Epoch string `json:"epoch"`
	Lane  Lane   `json:"lane"`
	Ops   []Op   `json:"ops"`
	// Content opens the content of the batch's files. It is nil on the
	// wire, where the spoke asks for content over the bulk session.
	Content ContentFunc `json:"-"`
}

// ResultStatus is how applying one op went.
type ResultStatus uint8

const (
	// Applied: the side holds the op's state now.
	Applied ResultStatus = iota + 1
	// Changed: the side holds content the hub has not seen there (a local
	// change, reported next), so the op was not applied.
	Changed
	// Stale: the content the op needs changed at its source; a newer op
	// follows.
	Stale
	// NoSpace: the side's disk cannot take the content (Reason says
	// which); the root is paused for growth on that side until room returns.
	NoSpace
	// Refused: the side will never apply this state (a symlink at a
	// reserved path, a path sharesync does not sync); it is not retried
	// until the path changes.
	Refused
	// Failed: anything else; retried with backoff.
	Failed
)

// String returns the status's name.
func (s ResultStatus) String() string {
	switch s {
	case Applied:
		return "applied"
	case Changed:
		return "changed"
	case Stale:
		return "stale"
	case NoSpace:
		return "no space"
	case Refused:
		return "refused"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Result is how applying one op went.
type Result struct {
	Path   string       `json:"p"`
	Rev    uint64       `json:"r"`
	Status ResultStatus `json:"s"`
	Reason string       `json:"reason,omitempty"`
}

// Peer is the hub's view of a side.
type Peer interface {
	// Apply applies the batch and returns one result per op. An error means
	// the side could not be reached; nothing is assumed applied.
	Apply(ctx context.Context, b Batch) ([]Result, error)
	// Fetch opens the content of the file path in root, which the side must
	// hold as e, from offset on. It fails with ErrStale when the side no
	// longer holds it.
	Fetch(ctx context.Context, root, path string, e Entry, offset int64) (io.ReadCloser, error)
	// Rescan scans the side's folders now and reports what changed, before
	// it returns.
	Rescan(ctx context.Context) error
}

// Reporter is a side's view of the hub.
type Reporter interface {
	// Report hands the hub a side's changes and returns its answers.
	Report(ctx context.Context, side string, r Report) (Reply, error)
}
