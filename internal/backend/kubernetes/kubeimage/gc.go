package kubeimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// gcGrace is the least time an image is unheld before it is collected, even
// with a retention of 0: it covers the moment between a delivery and its
// holder taking the image.
const gcGrace = time.Minute

// GCOptions steer one collection pass.
type GCOptions struct {
	// Live reports whether a holder still exists (an instance whose record
	// is gone does not); the pass first lets go for every holder it says is
	// gone. nil keeps every holder.
	Live func(holder string) bool
	// All collects every unheld image now, retention or not: the last pass
	// before a connection is removed, so its pushed images do not outlive
	// the index that knows them.
	All bool
}

// GCResult is what one collection pass did.
type GCResult struct {
	// Deleted are the images collected: deleted from the registry or the
	// nodes where the strategy allows, untagged on the daemon host, and
	// dropped from the index. What another connection of the daemon still
	// records (the same image in the same repository or local cluster) is
	// only dropped from this connection's index; the last connection to let
	// go deletes it.
	Deleted []Entry
	// Kept are images past retention the pass could not collect; each
	// one's GCError says why (a registry without DELETE, say). They are
	// tried again on the next pass. A kept pushed image whose registry
	// does not delete has lost its host tag all the same.
	Kept []Entry
	// Dropped are the holders Live said were gone.
	Dropped []string
	// Warnings are problems that did not stop the pass (the managed pull
	// Secret's refresh).
	Warnings []string
}

// GC collects the connection's images that nothing has held for its
// retention (Retention): pushed images are deleted from the registry by
// digest, only ever inside the daemon's own repository
// (<registry>/fleet/<daemon8>/devcontainer) and only where the registry
// deletes (CheckRegistry, re-checked daily); node-loaded images are removed
// from the nodes where the cluster's tool can (minikube; elsewhere the
// kubelet's image GC reclaims them); fleet's tags come off the daemon
// host's docker. An image the index does not know is never touched, so an
// index restored from an older backup cannot cost a running instance its
// image, and nothing another connection's index still records is deleted
// from where they share it. Collection of an image is serialized with its
// delivery, on every connection. The pass
// also refreshes the managed pull Secret (RefreshPullSecret), so the daemon's
// periodic GC keeps short-lived logins fresh in the cluster.
func (m *Manager) GC(ctx context.Context, t Target, opts GCOptions) (*GCResult, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	path, err := m.indexPath(t)
	if err != nil {
		return nil, err
	}
	res := &GCResult{}
	if opts.Live != nil {
		now := m.now()
		dropped := map[string]bool{}
		if err := m.files.update(path, func(ix *Index) error {
			for i := range ix.Images {
				for _, h := range append([]string(nil), ix.Images[i].Holders...) {
					if !opts.Live(h) {
						ix.Images[i].release(h, now)
						dropped[h] = true
					}
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		for h := range dropped {
			res.Dropped = append(res.Dropped, h)
		}
	}
	ix, err := m.files.read(path)
	if err != nil {
		return nil, err
	}
	retention := max(Retention(t.Cluster), gcGrace)
	if opts.All {
		retention = 0
	}
	for _, e := range ix.Images {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if len(e.Holders) > 0 || m.now().Sub(e.unheldSince()) < retention {
			continue
		}
		done, cur, err := m.collect(ctx, t, path, e, retention)
		switch {
		case err != nil:
			return res, err
		case done:
			res.Deleted = append(res.Deleted, cur)
		case cur.GCError != "":
			res.Kept = append(res.Kept, cur)
		}
	}
	if err := m.RefreshPullSecret(ctx, t); err != nil {
		res.Warnings = append(res.Warnings, "refreshing the managed pull Secret: "+err.Error())
		flog.Warn("kubernetes image gc: refreshing the managed pull secret failed", "cluster", t.Cluster.ID, "err", err)
	}
	if len(res.Deleted)+len(res.Kept)+len(res.Dropped) > 0 {
		flog.Info("kubernetes image gc", "cluster", t.Cluster.ID, "deleted", len(res.Deleted), "kept", len(res.Kept), "dropped_holders", len(res.Dropped))
	}
	return res, nil
}

// collect collects one entry under its lock, after checking again that it
// is still unheld and past retention. It reports whether the entry was
// collected; an entry it could not collect comes back with GCError set.
// Only a failure to update the index is an error.
func (m *Manager) collect(ctx context.Context, t Target, path string, snap Entry, retention time.Duration) (bool, Entry, error) {
	unlock := m.keys.lock(snap.lockKey())
	defer unlock()
	key := snap.key()
	ix, err := m.files.read(path)
	if err != nil {
		return false, Entry{}, err
	}
	e := ix.find(key)
	if e == nil || len(e.Holders) > 0 || m.now().Sub(e.unheldSince()) < retention {
		return false, Entry{}, nil
	}
	cur := *e
	why, untagged := "", false
	share, err := m.sharedWith(path, cur)
	if err != nil {
		why = "reading the other connections' image indexes: " + err.Error()
	} else {
		why, untagged = m.remove(ctx, t, ix, cur, share)
	}
	if why != "" {
		if why != cur.GCError {
			flog.Warn("kubernetes image gc kept an image", "cluster", t.Cluster.ID, "ref", cur.Ref, "reason", why)
		}
		cur.GCError = why
		if untagged {
			cur.Tag = ""
		}
		return false, cur, m.files.update(path, func(ix *Index) error {
			if e := ix.find(key); e != nil {
				e.GCError = why
				if untagged {
					e.Tag = ""
				}
			}
			return nil
		})
	}
	return true, cur, m.files.update(path, func(ix *Index) error {
		if e := ix.find(key); e != nil && len(e.Holders) == 0 {
			ix.remove(key)
		}
		return nil
	})
}

// sharing is what of an entry's leftovers another connection of the daemon
// still records, and so must stay: the registry manifest (the same
// repository and digest), the copy on a local cluster's nodes (the same
// cluster and image), the daemon host's tag.
type sharing struct {
	manifest, nodes, tag bool
}

// sharedWith reads the other connections' indexes for what e shares with
// them. Their deliveries of the same image hold its lock (lockKey) while they
// record it, so what they hold or are about to hold is always seen.
func (m *Manager) sharedWith(path string, e Entry) (sharing, error) {
	var s sharing
	if e.Strategy != StrategyRegistry && e.Strategy != StrategyNodeLoad {
		return s, nil
	}
	peers, err := peerIndexes(path)
	if err != nil {
		return s, err
	}
	for _, p := range peers {
		ix, err := m.files.read(p)
		if err != nil {
			return s, err
		}
		for _, o := range ix.Images {
			if o.Tag != "" && o.Tag == e.Tag {
				s.tag = true
			}
			if o.Strategy != e.Strategy || o.Location != e.Location {
				continue
			}
			switch e.Strategy {
			case StrategyRegistry:
				s.manifest = s.manifest || o.Digest == e.Digest
			case StrategyNodeLoad:
				s.nodes = s.nodes || o.ImageID == e.ImageID
			}
		}
	}
	return s, nil
}

// peerIndexes are the other connections' index files: the files named like
// path in the directories beside path's own.
func peerIndexes(path string) ([]string, error) {
	dir := filepath.Dir(path)
	parent, base := filepath.Dir(dir), filepath.Base(path)
	dirs, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		p := filepath.Join(parent, d.Name(), base)
		if !d.IsDir() || p == path {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return out, nil
}

// remove deletes what an entry left behind, except what another connection
// shares; it returns why it could not, or "", and whether it removed the
// daemon host's tag.
func (m *Manager) remove(ctx context.Context, t Target, ix *Index, e Entry, share sharing) (string, bool) {
	untag := func() string {
		if e.Tag == "" || share.tag {
			return ""
		}
		docker, err := m.docker()
		if err == nil {
			err = docker.untag(ctx, e.Tag)
		}
		if err != nil {
			return "removing the tag " + e.Tag + " on the daemon host: " + err.Error()
		}
		return ""
	}
	switch e.Strategy {
	case StrategyPassthrough:
		return "", false
	case StrategyNodeLoad:
		if e.Tag != "" && !isOwnTag(e.Tag, t.DaemonID, e.ImageID) {
			return e.Tag + " is not one of this daemon's tags; fleet removes only its own", false
		}
		c, err := parseLocal(e.Location)
		if err != nil {
			return err.Error(), false
		}
		if ld, err := m.loader(c, t); err == nil && !share.nodes {
			if err := ld.remove(ctx, io.Discard, e.Tag); err != nil && !errors.Is(err, errRemoveUnsupported) {
				return "removing the image from " + c.String() + ": " + err.Error(), false
			}
		}
		return untag(), false
	case StrategyRegistry:
		if e.Tag != "" && !isOwnTag(e.Tag, t.DaemonID, e.ImageID) {
			return e.Tag + " is not one of this daemon's tags; fleet removes only its own", false
		}
		why, cannot := "", false
		if !share.manifest {
			why, cannot = m.deleteFromRegistry(ctx, t, ix, e)
		}
		if why != "" && !cannot {
			return why, false
		}
		// A registry that cannot delete keeps the manifest, and the entry
		// stays for a later pass; the daemon host's tag was only there to
		// push from, so it goes now rather than pin the image on the host.
		if uwhy := untag(); uwhy != "" {
			return strings.TrimPrefix(why+"; "+uwhy, "; "), false
		}
		return why, why != "" && e.Tag != "" && !share.tag
	case StrategyGateway:
		if m.Gateway == nil {
			return "gateway delivery is not available in this daemon", false
		}
		if err := m.Gateway.Remove(ctx, t, e); err != nil {
			return err.Error(), false
		}
		return "", false
	}
	return fmt.Sprintf("unknown strategy %q", e.Strategy), false
}

// deleteFromRegistry deletes a pushed image's manifest. It returns why it
// did not, or "", and whether that is because the registry does not delete
// for the daemon host's login (rather than a failure a later pass may not
// meet).
func (m *Manager) deleteFromRegistry(ctx context.Context, t Target, ix *Index, e Entry) (string, bool) {
	repo, err := ParseReference(e.Location)
	if err != nil {
		return err.Error(), false
	}
	if !isOwnRepository(e.Location, t.DaemonID) {
		return e.Location + " is not this daemon's repository; fleet deletes only inside fleet/" + daemon8(t.DaemonID) + "/devcontainer", false
	}
	if !digestPattern.MatchString(e.Digest) {
		return "no digest recorded for " + e.Ref, false
	}
	state, known := ix.Registries[repo.Domain]
	if known && state.DeleteSupported != nil && !*state.DeleteSupported && m.now().Sub(state.CheckedAt) <= deleteRecheck {
		return state.Reason, true
	}
	c, err := m.client(ctx, t, repo.Domain)
	if err != nil {
		return err.Error(), false
	}
	if !known || state.DeleteSupported == nil || m.now().Sub(state.CheckedAt) > deleteRecheck {
		// The entry was pushed, so the daemon's repository exists and the
		// answer stands.
		supported, why, _ := probeDelete(ctx, c, ownPath(registryPrefix(repo.Path), t.DaemonID))
		state = RegistryState{DeleteSupported: &supported, Reason: why, CheckedAt: m.now(), Pushed: true}
		if path, err := m.indexPath(t); err == nil {
			_ = m.files.update(path, func(ix *Index) error {
				if ix.Registries == nil {
					ix.Registries = map[string]RegistryState{}
				}
				ix.Registries[repo.Domain] = state
				return nil
			})
		}
	}
	if !*state.DeleteSupported {
		return state.Reason, true
	}
	if err := c.deleteManifest(ctx, repo.Path, e.Digest); err != nil && !isNotFound(err) {
		return "deleting " + e.Ref + ": " + err.Error(), false
	}
	return "", false
}

// registryPrefix is the setting's path prefix of one of the daemon's
// repository paths ("team/fleet/<d8>/devcontainer" → "team").
func registryPrefix(repoPath string) string {
	parts := strings.Split(repoPath, "/")
	if len(parts) <= 3 {
		return ""
	}
	return strings.Join(parts[:len(parts)-3], "/")
}
