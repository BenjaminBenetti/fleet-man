package kubeimage

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// dropped from the index.
	Deleted []Entry
	// Kept are images past retention the pass could not collect; each
	// one's GCError says why (a registry without DELETE, say). They are
	// tried again on the next pass.
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
// image. Collection of an image is serialized with its delivery. The pass
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
		done, cur, err := m.collect(ctx, t, path, e.key(), retention)
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

// collect collects one entry under its key's lock, after checking again
// that it is still unheld and past retention. It reports whether the entry
// was collected; an entry it could not collect comes back with GCError set.
// Only a failure to update the index is an error.
func (m *Manager) collect(ctx context.Context, t Target, path, key string, retention time.Duration) (bool, Entry, error) {
	unlock := m.keys.lock(path + "|" + key)
	defer unlock()
	ix, err := m.files.read(path)
	if err != nil {
		return false, Entry{}, err
	}
	e := ix.find(key)
	if e == nil || len(e.Holders) > 0 || m.now().Sub(e.unheldSince()) < retention {
		return false, Entry{}, nil
	}
	cur := *e
	why := m.remove(ctx, t, ix, cur)
	if why != "" {
		cur.GCError = why
		flog.Warn("kubernetes image gc kept an image", "cluster", t.Cluster.ID, "ref", cur.Ref, "reason", why)
		return false, cur, m.files.update(path, func(ix *Index) error {
			if e := ix.find(key); e != nil {
				e.GCError = why
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

// remove deletes what an entry left behind; it returns why it could not, or
// "".
func (m *Manager) remove(ctx context.Context, t Target, ix *Index, e Entry) string {
	untag := func() string {
		if e.Tag == "" {
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
		return ""
	case StrategyNodeLoad:
		c, err := parseLocal(e.Location)
		if err != nil {
			return err.Error()
		}
		if ld, err := m.loader(c, t); err == nil {
			if err := ld.remove(ctx, io.Discard, e.Tag); err != nil && !errors.Is(err, errRemoveUnsupported) {
				return "removing the image from " + c.String() + ": " + err.Error()
			}
		}
		return untag()
	case StrategyRegistry:
		if why := m.deleteFromRegistry(ctx, t, ix, e); why != "" {
			return why
		}
		return untag()
	case StrategyGateway:
		if m.Gateway == nil {
			return "gateway delivery is not available in this daemon"
		}
		if err := m.Gateway.Remove(ctx, t, e); err != nil {
			return err.Error()
		}
		return ""
	}
	return fmt.Sprintf("unknown strategy %q", e.Strategy)
}

// deleteFromRegistry deletes a pushed image's manifest; it returns why it
// did not, or "".
func (m *Manager) deleteFromRegistry(ctx context.Context, t Target, ix *Index, e Entry) string {
	repo, err := ParseReference(e.Location)
	if err != nil {
		return err.Error()
	}
	if !isOwnRepository(e.Location, t.DaemonID) {
		return e.Location + " is not this daemon's repository; fleet deletes only inside fleet/" + daemon8(t.DaemonID) + "/devcontainer"
	}
	if !digestPattern.MatchString(e.Digest) {
		return "no digest recorded for " + e.Ref
	}
	c, err := m.client(ctx, t, repo.Domain)
	if err != nil {
		return err.Error()
	}
	state, known := ix.Registries[repo.Domain]
	if !known || state.DeleteSupported == nil || m.now().Sub(state.CheckedAt) > deleteRecheck {
		supported, why := probeDelete(ctx, c, ownPath(registryPrefix(repo.Path), t.DaemonID))
		state = RegistryState{DeleteSupported: &supported, Reason: why, CheckedAt: m.now()}
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
		return state.Reason
	}
	if err := c.deleteManifest(ctx, repo.Path, e.Digest); err != nil && !isNotFound(err) {
		return "deleting " + e.Ref + ": " + err.Error()
	}
	return ""
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
