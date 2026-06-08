package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deleteImageCache is the cache-wipe seam (a package var so the RPC handler can
// be tested without docker).
var deleteImageCache = imagecache.DeleteCache

// DeleteImageCache wipes a fleet's shared docker image cache and restarts the
// empty server. Synchronous; mirrors DeleteBuildkitCache.
func (s *service) DeleteImageCache(_ context.Context, req *fleetgrpc.DeleteImageCacheRequest) (*fleetgrpc.DeleteImageCacheReply, error) {
	fleetName := req.GetFleet()
	if fleetName == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet is required")
	}
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	if !f.Settings.ImageCacheServer {
		return nil, status.Errorf(codes.FailedPrecondition, "fleet %q does not have the image cache enabled", fleetName)
	}
	if err := deleteImageCache(fleetName); err != nil {
		return nil, status.Errorf(codes.Internal, "delete image cache: %v", err)
	}
	return &fleetgrpc.DeleteImageCacheReply{}, nil
}

// ensureImageCacheServer is the re-ensure seam (a package var so the TUI-connect
// sweep can be exercised in tests without docker).
var ensureImageCacheServer = imagecache.EnsureSharedServer

// ensureConfiguredImageCacheServers re-ensures the shared image cache server for
// every fleet that has the feature enabled AND has at least one instance on a
// backend that SupportsCustomMounts. Mirrors ensureConfiguredBuildkitServers.
func ensureConfiguredImageCacheServers(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("image cache reconcile: load state failed", "err", err)
		return
	}
	for name, f := range st.Fleets {
		if ctx.Err() != nil {
			return
		}
		if f == nil || !f.Settings.ImageCacheServer {
			continue
		}
		if !fleetHasCustomMountInstance(f) {
			continue
		}
		if _, err := ensureImageCacheServer(name); err != nil {
			flog.Warn("image cache reconcile: ensure failed", "fleet", name, "err", err)
		}
	}
}
