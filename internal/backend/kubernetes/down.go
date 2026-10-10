//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// down.go removes an instance from its cluster by its key, which every one
// of its objects carries: the teardown of a record with no ContainerID (a
// create that failed or was interrupted after its first apply), and of any
// other, since the key reaches objects a ContainerID never names (Jobs,
// Secrets of older generations, snapshots).

// OwnerGate answers whether this daemon may delete objects on connection
// clusterID now: nil while it holds the connection's owner Lease,
// ErrOwnerConflict (or why it cannot tell) otherwise.
type OwnerGate func(ctx context.Context, clusterID string) error

// SetOwnerGate installs the owner check every deletion of the pool's runs
// first (fleetd's: the connection's owner Lease, internal/server). Without
// one, deletions are not checked.
func (p *Pool) SetOwnerGate(gate OwnerGate) {
	p.hookMu.Lock()
	p.ownerGate = gate
	p.hookMu.Unlock()
}

func (p *Pool) gateFor(clusterID string) func(context.Context) error {
	p.hookMu.Lock()
	gate := p.ownerGate
	if gate == nil {
		gate = p.opts.OwnerGate
	}
	p.hookMu.Unlock()
	if gate == nil {
		return nil
	}
	return func(ctx context.Context) error { return gate(ctx, clusterID) }
}

// Scope is connection clusterID's namespace, as this daemon's objects in it
// are reached, with the pool's owner check.
func (p *Pool) Scope(clusterID string) (Scope, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return Scope{}, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return Scope{}, err
	}
	own, err := p.owner()
	if err != nil {
		return Scope{}, err
	}
	return Scope{Client: cs, Namespace: c.Namespace(), Owner: own, Cluster: clusterID, Gate: p.gateFor(clusterID), Logger: p.opts.Logger}, nil
}

// DeleteByKey deletes every object of instance key on connection clusterID
// and the key's job journal: what a retry of a failed create clears before
// it creates again under the same key.
func (p *Pool) DeleteByKey(ctx context.Context, clusterID, key string) (Teardown, error) {
	if !names.ValidHash(key) {
		return Teardown{}, fmt.Errorf("%q is not an instance key", key)
	}
	s, err := p.Scope(clusterID)
	if err != nil {
		return Teardown{}, err
	}
	t, err := s.DeleteByKey(ctx, key)
	if err != nil {
		return t, err
	}
	return t, removeJournalOf(clusterID, key)
}

// DownByKey is a destroy's teardown of an instance by its key: the key's
// tombstone first (footprint.go), so whatever this teardown cannot delete,
// now or ever (the cluster is unreachable, the daemon dies halfway, a
// cancelled create applies one more object later), is known garbage the
// reconciler deletes once it can; then every object of the key and its job
// journal. The tombstone stays until the reconciler finds nothing of the
// key left. A tombstone that cannot be written (a newer fleet's footprint,
// a full disk) does not stop the teardown, which is then all there is.
func (p *Pool) DownByKey(ctx context.Context, t Tombstone) (Teardown, error) {
	if !names.ValidHash(t.Key) {
		return Teardown{}, fmt.Errorf("%q is not an instance key", t.Key)
	}
	tombErr := AddTombstone(t)
	if tombErr != nil {
		p.opts.Logger.Warn("kube: recording a destroyed instance's key", "cluster", t.Cluster, "key", t.Key, "err", tombErr)
	}
	td, err := p.DeleteByKey(ctx, t.Cluster, t.Key)
	switch {
	case err != nil && tombErr != nil:
		return td, fmt.Errorf("%w (and fleet could not record the key to delete what is left later: %v)", err, tombErr)
	case err != nil:
		return td, fmt.Errorf("%w (fleet deletes what is left once the cluster lets it)", err)
	}
	return td, nil
}

// removeJournalOf removes the job journal of key when it is clusterID's.
func removeJournalOf(clusterID, key string) error {
	path := JournalPath(key)
	j, err := ReadJournal(path)
	if err != nil && j.Cluster == "" {
		// Unreadable, or none: nothing of clusterID's to remove (a
		// journal another fleet wrote is left alone).
		return nil
	}
	if j.Cluster != clusterID {
		return nil
	}
	return RemoveJournal(path)
}
