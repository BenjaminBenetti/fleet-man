package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deleteDebCache is the cache-wipe seam (a package var so the RPC handler can be
// tested without docker).
var deleteDebCache = debcache.DeleteCache

// DeleteDebCache wipes a fleet's shared deb (apt) cache and restarts the empty
// server. Synchronous: it returns only once the cache is gone and the server is
// back. Mirrors DeleteBuildkitCache.
func (s *service) DeleteDebCache(_ context.Context, req *fleetgrpc.DeleteDebCacheRequest) (*fleetgrpc.DeleteDebCacheReply, error) {
	fleetName := req.GetFleet()
	if fleetName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet is required")
	}
	// Guard: only wipe for a fleet that actually has the deb cache enabled, so
	// DeleteCache's restart step never spins up a server the fleet never asked for.
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	if !f.Settings.DebCacheServer {
		return nil, status.Errorf(codes.FailedPrecondition, "fleet %q does not have the deb cache enabled", fleetName)
	}
	if err := deleteDebCache(fleetName); err != nil {
		return nil, status.Errorf(codes.Internal, "delete deb cache: %v", err)
	}
	return &fleetgrpc.DeleteDebCacheReply{}, nil
}

// ensureDebCacheServer is the re-ensure seam (a package var so the TUI-connect
// sweep can be exercised in tests without docker).
var ensureDebCacheServer = debcache.EnsureSharedServer

// ensureConfiguredDebCacheServers re-ensures the shared deb cache server for
// every fleet that has the feature enabled AND has at least one instance on a
// backend that SupportsCustomMounts. Fire-and-forget reconciliation kicked off
// when a TUI connects (see service.onTUIConnected); mirrors
// ensureConfiguredBuildkitServers and reuses fleetHasCustomMountInstance.
func ensureConfiguredDebCacheServers(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("deb cache reconcile: load state failed", "err", err)
		return
	}
	for name, f := range st.Fleets {
		if ctx.Err() != nil {
			return
		}
		if f == nil || !f.Settings.DebCacheServer {
			continue
		}
		if !fleetHasCustomMountInstance(f) {
			continue
		}
		if _, err := ensureDebCacheServer(name); err != nil {
			flog.Warn("deb cache reconcile: ensure failed", "fleet", name, "err", err)
		}
	}
}
