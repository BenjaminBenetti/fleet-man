package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// footprint.go is ~/.fleet/kube/footprint.json, what the daemon remembers of
// its own doings in its clusters beyond the instance records:
//
//   - the tombstones: the key of every instance this daemon destroyed whose
//     objects are not confirmed gone yet. A destroy writes the tombstone
//     before it deletes anything, so objects a deletion missed (the cluster
//     was unreachable, the daemon died halfway, a cancelled create applied
//     one more object after the teardown) are known garbage, which the
//     reconciler deletes once the cluster answers, under the owner Lease,
//     without waiting for `fleet cluster gc --apply`;
//   - the fleets: every fleet the daemon gave objects of its own on a
//     connection (internal/svcruntime/kubert: the anchor, sibling DNS, the
//     NetworkPolicies, the caches), with the teardown of them it still owes
//     there (PendingTeardown) when the fleet left the cluster while the
//     cluster could not be reached. The reconciler finishes those too.
//
// The file is versioned. Fields this fleet does not know are kept as they
// are, so an older fleet never drops what a newer one recorded; a file of a
// newer version is read but never written.

// FootprintVersion is the footprint format this fleet writes.
const FootprintVersion = 1

// maxTombstones bounds the file: past it the oldest tombstones go (their
// objects, if any are left, are then unknown orphans `fleet cluster gc`
// lists).
const maxTombstones = 1000

// ErrFootprintNewer is a footprint a newer fleet wrote: read-only to this one.
var ErrFootprintNewer = errors.New("the Kubernetes footprint was written by a newer fleet")

// Tombstone is a destroyed instance whose objects may not all be gone.
type Tombstone struct {
	Cluster     string    `json:"cluster"`
	Key         string    `json:"key"`
	Fleet       string    `json:"fleet,omitempty"`
	Instance    string    `json:"inst,omitempty"`
	DestroyedAt time.Time `json:"destroyedAt"`
}

// FleetPresence is a fleet on a connection: the daemon made the fleet's own
// objects there (named by its h8, names.FleetHash), and its cache volumes may
// still be there after its instances left.
type FleetPresence struct {
	Cluster string `json:"cluster"`
	Fleet   string `json:"fleet"`
	// Hash is the fleet's h8 on the connection, which labels its objects.
	Hash string `json:"h8"`
	// PreparedAt is when the fleet's objects were last applied.
	PreparedAt time.Time `json:"preparedAt"`
	// PendingTeardown is a teardown of the fleet's objects on the connection
	// that could not finish: the fleet left the cluster (its last instance
	// there, or the fleet itself, was destroyed) while the cluster could not
	// be reached or refused. Nil when nothing is owed.
	PendingTeardown *PendingTeardown `json:"pendingTeardown,omitempty"`
}

// PendingTeardown is a fleet teardown the daemon still owes a connection.
type PendingTeardown struct {
	Since time.Time `json:"since"`
	// DeleteCaches deletes the fleet's cache volumes too (a fleet destroy on
	// a connection that deletes them with the fleet); otherwise they stay
	// for the next fleet of the same name.
	DeleteCaches bool `json:"deleteCaches,omitempty"`
	// FleetGone: the fleet itself was destroyed, so its presence goes once
	// the teardown is done.
	FleetGone bool `json:"fleetGone,omitempty"`
	// Error is why the last attempt did not finish.
	Error string `json:"error,omitempty"`
}

// Footprint is footprint.json.
type Footprint struct {
	V          int
	Tombstones []Tombstone
	Fleets     []FleetPresence
	// extra holds the top-level fields this fleet does not know.
	extra map[string]json.RawMessage
}

// Presence is fleetName's presence on cluster, or nil.
func (f *Footprint) Presence(cluster, fleetName string) *FleetPresence {
	for i := range f.Fleets {
		if f.Fleets[i].Cluster == cluster && f.Fleets[i].Fleet == fleetName {
			return &f.Fleets[i]
		}
	}
	return nil
}

// SetPresence records p, replacing the presence of the same fleet on the
// same connection.
func (f *Footprint) SetPresence(p FleetPresence) {
	if cur := f.Presence(p.Cluster, p.Fleet); cur != nil {
		*cur = p
		return
	}
	f.Fleets = append(f.Fleets, p)
}

// DropPresence forgets fleetName on cluster, reporting whether it was there.
func (f *Footprint) DropPresence(cluster, fleetName string) bool {
	for i, p := range f.Fleets {
		if p.Cluster == cluster && p.Fleet == fleetName {
			f.Fleets = append(f.Fleets[:i], f.Fleets[i+1:]...)
			return true
		}
	}
	return false
}

// PresencesOf are the fleets on cluster.
func (f *Footprint) PresencesOf(cluster string) []FleetPresence {
	var out []FleetPresence
	for _, p := range f.Fleets {
		if p.Cluster == cluster {
			out = append(out, p)
		}
	}
	return out
}

// ClustersOf are the connections fleetName is present on, in record order.
func (f *Footprint) ClustersOf(fleetName string) []string {
	var out []string
	for _, p := range f.Fleets {
		if p.Fleet == fleetName {
			out = append(out, p.Cluster)
		}
	}
	return out
}

// AddTombstone records t, replacing a tombstone of the same key.
func (f *Footprint) AddTombstone(t Tombstone) {
	f.DropTombstone(t.Cluster, t.Key)
	f.Tombstones = append(f.Tombstones, t)
	if n := len(f.Tombstones) - maxTombstones; n > 0 {
		sort.SliceStable(f.Tombstones, func(a, b int) bool {
			return f.Tombstones[a].DestroyedAt.Before(f.Tombstones[b].DestroyedAt)
		})
		f.Tombstones = append([]Tombstone(nil), f.Tombstones[n:]...)
	}
}

// DropTombstone removes the tombstone of key on cluster, reporting whether
// there was one.
func (f *Footprint) DropTombstone(cluster, key string) bool {
	for i, t := range f.Tombstones {
		if t.Cluster == cluster && t.Key == key {
			f.Tombstones = append(f.Tombstones[:i], f.Tombstones[i+1:]...)
			return true
		}
	}
	return false
}

// TombstonesOf are the tombstones on cluster.
func (f *Footprint) TombstonesOf(cluster string) []Tombstone {
	var out []Tombstone
	for _, t := range f.Tombstones {
		if t.Cluster == cluster {
			out = append(out, t)
		}
	}
	return out
}

func (f *Footprint) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range f.extra {
		m[k] = v
	}
	m["v"] = f.V
	m["tombstones"] = f.Tombstones
	if f.Tombstones == nil {
		m["tombstones"] = []Tombstone{}
	}
	if len(f.Fleets) > 0 {
		m["fleets"] = f.Fleets
	}
	return json.Marshal(m)
}

func (f *Footprint) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*f = Footprint{}
	if raw, ok := m["v"]; ok {
		if err := json.Unmarshal(raw, &f.V); err != nil {
			return fmt.Errorf("v: %w", err)
		}
	}
	if raw, ok := m["tombstones"]; ok {
		if err := json.Unmarshal(raw, &f.Tombstones); err != nil {
			return fmt.Errorf("tombstones: %w", err)
		}
	}
	if raw, ok := m["fleets"]; ok {
		if err := json.Unmarshal(raw, &f.Fleets); err != nil {
			return fmt.Errorf("fleets: %w", err)
		}
	}
	delete(m, "v")
	delete(m, "tombstones")
	delete(m, "fleets")
	if len(m) > 0 {
		f.extra = m
	}
	return nil
}

// footprintMu serializes this process's footprint updates.
var footprintMu sync.Mutex

// FootprintPath is where the footprint is (a var for tests).
var FootprintPath = state.KubeFootprintPath

// LoadFootprint reads the footprint; a missing file is an empty one. A
// newer file comes back with ErrFootprintNewer as well, for reading only.
func LoadFootprint() (*Footprint, error) {
	return loadFootprint(FootprintPath())
}

func loadFootprint(path string) (*Footprint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Footprint{V: FootprintVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kube footprint: %w", err)
	}
	f := &Footprint{}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("kube footprint %s: %w", path, err)
	}
	if f.V > FootprintVersion {
		return f, fmt.Errorf("%w (version %d)", ErrFootprintNewer, f.V)
	}
	return f, nil
}

// UpdateFootprint changes the footprint with fn under this process's lock
// and writes it (0600) when fn returns nil (ErrFootprintUnchanged: no write,
// no error). A newer file is refused before fn runs.
func UpdateFootprint(fn func(*Footprint) error) error {
	footprintMu.Lock()
	defer footprintMu.Unlock()
	path := FootprintPath()
	f, err := loadFootprint(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		if errors.Is(err, ErrFootprintUnchanged) {
			return nil
		}
		return err
	}
	f.V = FootprintVersion
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("kube footprint: %w", err)
	}
	if err := atomicfile.Write(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("kube footprint: %w", err)
	}
	return nil
}

// AddTombstone records a destroyed instance's key (see the file comment).
func AddTombstone(t Tombstone) error {
	if t.DestroyedAt.IsZero() {
		t.DestroyedAt = time.Now().UTC()
	}
	return UpdateFootprint(func(f *Footprint) error {
		f.AddTombstone(t)
		return nil
	})
}

// DropTombstone forgets the tombstone of key on cluster: nothing of it is
// left in the cluster, or a record holds the key again.
func DropTombstone(cluster, key string) error {
	return UpdateFootprint(func(f *Footprint) error {
		if !f.DropTombstone(cluster, key) {
			return ErrFootprintUnchanged
		}
		return nil
	})
}

// DropTombstoneOf is DropTombstone of t, the tombstone as it was read: one
// a later destroy of the key wrote since (another DestroyedAt) is kept.
func DropTombstoneOf(t Tombstone) error {
	return UpdateFootprint(func(f *Footprint) error {
		for _, cur := range f.Tombstones {
			if cur.Cluster == t.Cluster && cur.Key == t.Key {
				if !cur.DestroyedAt.Equal(t.DestroyedAt) {
					return ErrFootprintUnchanged
				}
				f.DropTombstone(t.Cluster, t.Key)
				return nil
			}
		}
		return ErrFootprintUnchanged
	})
}

// RecordFleetPrepared records that fleetName's objects on cluster (h8 hash)
// were just applied: the fleet is there, and any teardown owed there is void.
func RecordFleetPrepared(cluster, fleetName, hash string, at time.Time) error {
	return UpdateFootprint(func(f *Footprint) error {
		f.SetPresence(FleetPresence{Cluster: cluster, Fleet: fleetName, Hash: hash, PreparedAt: at.UTC()})
		return nil
	})
}

// RecordPendingTeardown records a teardown of fleetName's objects on cluster
// that could not finish (why), keeping the presence's other fields; the
// flags of a teardown owed already are kept when this one asks for less.
func RecordPendingTeardown(cluster, fleetName, hash string, t PendingTeardown) error {
	if t.Since.IsZero() {
		t.Since = time.Now().UTC()
	}
	return UpdateFootprint(func(f *Footprint) error {
		p := FleetPresence{Cluster: cluster, Fleet: fleetName, Hash: hash}
		if cur := f.Presence(cluster, fleetName); cur != nil {
			p = *cur
			if p.Hash == "" {
				p.Hash = hash
			}
			if old := cur.PendingTeardown; old != nil {
				t.Since = old.Since
				t.DeleteCaches = t.DeleteCaches || old.DeleteCaches
				t.FleetGone = t.FleetGone || old.FleetGone
			}
		}
		p.PendingTeardown = &t
		f.SetPresence(p)
		return nil
	})
}

// SettleFleetTeardown ends fleetName's presence on cluster after a teardown
// finished: forgotten when its fleet is gone (gone), else kept without a
// pending teardown (its cache volumes stay there).
func SettleFleetTeardown(cluster, fleetName string, gone bool) error {
	return UpdateFootprint(func(f *Footprint) error {
		if gone {
			if !f.DropPresence(cluster, fleetName) {
				return ErrFootprintUnchanged
			}
			return nil
		}
		p := f.Presence(cluster, fleetName)
		if p == nil || p.PendingTeardown == nil {
			return ErrFootprintUnchanged
		}
		p.PendingTeardown = nil
		return nil
	})
}

// ForgetClusterFleets drops every fleet presence on cluster: the connection
// is gone, and with it any way to reach what is left there.
func ForgetClusterFleets(cluster string) error {
	return UpdateFootprint(func(f *Footprint) error {
		kept := f.Fleets[:0]
		for _, p := range f.Fleets {
			if p.Cluster != cluster {
				kept = append(kept, p)
			}
		}
		if len(kept) == len(f.Fleets) {
			return ErrFootprintUnchanged
		}
		f.Fleets = kept
		return nil
	})
}

// ErrFootprintUnchanged, returned by an UpdateFootprint fn, ends the update
// without writing and without an error.
var ErrFootprintUnchanged = errors.New("footprint unchanged")
