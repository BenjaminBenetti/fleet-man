//go:build !nok8s

package server

import (
	"context"
	"errors"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// share_sync_rpc.go serves `fleet sync status|wait|conflicts`: the
// shared-state sync's RPCs, to remote clients exactly as to local ones.

// shareWaitMax caps a WaitShareSync's timeout.
const shareWaitMax = time.Hour

// GetShareSync reports each fleet's sync.
func (s *service) GetShareSync(ctx context.Context, req *fleetgrpc.GetShareSyncRequest) (*fleetgrpc.GetShareSyncReply, error) {
	if err := shareFleetExists(req.GetFleet()); err != nil {
		return nil, err
	}
	reply := &fleetgrpc.GetShareSyncReply{}
	for _, f := range s.shareSync.report(ctx, req.GetFleet()) {
		reply.Fleets = append(reply.Fleets, protoconv.ShareSyncToProto(f))
	}
	return reply, nil
}

// WaitShareSync waits until the fleet's sides agree.
func (s *service) WaitShareSync(ctx context.Context, req *fleetgrpc.WaitShareSyncRequest) (*fleetgrpc.WaitShareSyncReply, error) {
	if err := shareFleetExists(req.GetFleet()); err != nil {
		return nil, err
	}
	timeout := shareWaitDefault
	if ms := req.GetTimeoutMs(); ms > 0 {
		timeout = min(time.Duration(ms)*time.Millisecond, shareWaitMax)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pending := s.shareSync.wait(ctx, req.GetFleet())
	return &fleetgrpc.WaitShareSyncReply{Synced: len(pending) == 0, Pending: pending}, nil
}

// ListShareConflicts lists the conflict copies kept.
func (s *service) ListShareConflicts(ctx context.Context, req *fleetgrpc.ListShareConflictsRequest) (*fleetgrpc.ListShareConflictsReply, error) {
	if err := shareFleetExists(req.GetFleet()); err != nil {
		return nil, err
	}
	reply := &fleetgrpc.ListShareConflictsReply{}
	for _, c := range s.shareSync.conflicts(req.GetFleet()) {
		reply.Conflicts = append(reply.Conflicts, protoconv.ShareConflictToProto(c))
	}
	return reply, nil
}

// shareFleetExists refuses a fleet that is not there ("" is every fleet).
func shareFleetExists(name string) error {
	if name == "" {
		return nil
	}
	st, err := state.Load()
	if err != nil {
		return status.Errorf(codes.Internal, "load state: %v", err)
	}
	if _, ok := st.Fleets[name]; !ok {
		return status.Errorf(codes.NotFound, "fleet %q not found", name)
	}
	return nil
}

// openFleets are the fleets with a sync (fleetName's alone when set), in
// name order.
func (m *shareSync) openFleets(fleetName string) []*fleetSync {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*fleetSync
	for name, fs := range m.fleets {
		if fleetName == "" || name == fleetName {
			out = append(out, fs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// report is each fleet's sync (fleetName's alone when set), with its
// shares as full as they are.
func (m *shareSync) report(ctx context.Context, fleetName string) []fleet.ShareSync {
	if fleetName != "" {
		// A fleet just given an instance on a cluster: its sync opens now.
		m.reconcile(ctx)
	}
	var out []fleet.ShareSync
	for _, fs := range m.openFleets(fleetName) {
		r := shareSyncOf(fs.name, fs.hub.Status())
		if all, err := fs.hub.Conflicts(); err == nil {
			r.Conflicts = len(all)
		}
		m.recMu.Lock()
		clusters := map[string]bool{}
		for _, sw := range fs.shares {
			if sw.share.Claim != "" {
				clusters[sw.cluster] = true
			}
		}
		m.recMu.Unlock()
		ids := make([]string, 0, len(clusters))
		for id := range clusters {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			uctx, cancel := context.WithTimeout(ctx, shareUsageTimeout)
			u, ok, err := m.spokes.usage(uctx, id, fs.name)
			cancel()
			if err != nil {
				m.log.Info("share sync: measuring a share", "fleet", fs.name, "cluster", id, "err", err)
				if rec, found := m.recordOf(id); found {
					u = fleet.ShareUsage{Cluster: id, Location: clusterLabel(rec), Fleet: fs.name, Warning: "not measured: " + err.Error()}
					ok = true
				}
			}
			if ok {
				r.Shares = append(r.Shares, u)
			}
		}
		out = append(out, r)
	}
	return out
}

// shareSyncOf is a hub's status as `fleet sync status` shows it.
func shareSyncOf(name string, st sharesync.Status) fleet.ShareSync {
	out := fleet.ShareSync{Fleet: name, Paused: st.PauseReasons()}
	for _, r := range st.Roots {
		root := fleet.ShareSyncRoot{
			Root: r.Root, Files: r.Files, Bytes: r.Bytes, Cap: r.Cap,
			Paused: r.Paused, Broken: r.Broken, Warnings: r.Warnings,
		}
		for _, side := range r.Sides {
			root.Sides = append(root.Sides, fleet.ShareSyncSide{
				Side: side.Side, Location: side.Location, Pending: side.Pending, PendingBytes: side.PendingBytes,
				Paused: side.Paused, Refused: side.Refused, LastError: side.LastError,
			})
		}
		out.Roots = append(out.Roots, root)
	}
	return out
}

// wait waits, within ctx, until every side of fleetName's sync (every
// fleet's for "") agrees, and returns what was still behind (nil: in sync).
// A side that cannot rescan (a spoke reconnecting) is tried again while
// there is time.
func (m *shareSync) wait(ctx context.Context, fleetName string) []string {
	m.reconcile(ctx)
	var names []string
	if fleetName != "" {
		names = []string{fleetName}
	} else {
		for _, fs := range m.openFleets("") {
			names = append(names, fs.name)
		}
	}
	var pending []string
	for _, name := range names {
		var err error
		for {
			if err = m.flushFleet(ctx, name); err == nil || ctx.Err() != nil {
				break
			}
			var fe *sharesync.FlushError
			if !errors.As(err, &fe) {
				break
			}
			// What cannot move now (a spoke reconnecting, a share not
			// attached yet, a full disk) may in a moment.
			t := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
			if ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			pending = append(pending, flushLines(name, err, len(names) > 1)...)
		}
	}
	return pending
}

// flushLines are a failed flush's lines, each named by its fleet when
// several are waited for.
func flushLines(fleetName string, err error, named bool) []string {
	var lines []string
	var fe *sharesync.FlushError
	if errors.As(err, &fe) {
		lines = append(lines, fe.Pending...)
	} else {
		lines = append(lines, err.Error())
	}
	if named {
		for i := range lines {
			lines[i] = fleetName + ": " + lines[i]
		}
	}
	return lines
}

// conflicts are the conflict copies of fleetName's sync (every fleet's for
// ""), oldest first.
func (m *shareSync) conflicts(fleetName string) []fleet.ShareConflict {
	var out []fleet.ShareConflict
	for _, fs := range m.openFleets(fleetName) {
		all, err := fs.hub.Conflicts()
		if err != nil {
			m.log.Warn("share sync: reading the conflict copies", "fleet", fs.name, "err", err)
			continue
		}
		for _, c := range all {
			out = append(out, fleet.ShareConflict{
				ID: c.ID, Time: c.Time, Fleet: fs.name, Root: c.Root, Path: c.Path, Location: c.Location,
				Side: c.Side, Reason: c.Reason, Size: c.Entry.Size, File: c.File,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}
