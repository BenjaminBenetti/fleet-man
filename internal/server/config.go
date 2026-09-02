package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// config.go implements the server-owned config.json RPCs. config.json is folded
// into the same single-writer ownership as state.json (the muWrite lock) so it
// gets the same atomic-save guarantee and no torn-write/lost-update exposure.
//
// The config proto faithfully mirrors internal/state.Config field-for-field, so
// SetConfig round-trips the FULL config (browser tri-states + rich coder
// parameters included) without loss.

// GetConfig returns the current config (defaults when config.json is absent).
func (s *service) GetConfig(_ context.Context, _ *fleetgrpc.GetConfigRequest) (*fleetgrpc.GetConfigReply, error) {
	c, err := state.LoadConfig()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load config: %v", err)
	}
	return &fleetgrpc.GetConfigReply{Config: protoconv.ConfigToProto(c)}, nil
}

// SetConfig replaces the whole config (the settings page sends the full edited
// Config). It returns the post-save config so the caller picks up SaveConfig's
// applyDefaults() normalization (e.g. an unknown agent tool snapped to claude).
// reconcileRemote converges both remote-control transports on the settings:
// the gateway tunnel negotiates grpc only in gateway mode, and the SSH loopback
// listener is up only in SSH mode — so flipping the mode moves the surface
// rather than doubling it. nil-safe for tests that use newService() without a
// serve loop.
func (s *service) reconcileRemote(rm state.RemoteMcpSettings) {
	if s.remote != nil {
		s.remote.Reconcile(rm.Enabled, rm.FleetViaGateway(), rm.WebhookEnabled, rm.GatewayURL)
	}
	if s.sshListen != nil {
		s.sshListen.Reconcile(rm.FleetViaSSH())
	}
}

func (s *service) SetConfig(_ context.Context, req *fleetgrpc.SetConfigRequest) (*fleetgrpc.SetConfigReply, error) {
	s.muWrite.Lock()
	defer s.muWrite.Unlock()

	if err := state.SaveConfig(protoconv.ConfigFromProto(req.GetConfig(), &state.Config{})); err != nil {
		return nil, status.Errorf(codes.Internal, "save config: %v", err)
	}
	saved, err := state.LoadConfig()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload config: %v", err)
	}

	// Converge the remote transports (gateway tunnel + SSH listener) to the
	// saved settings. Both reconciles are non-blocking / quick, so calling them
	// while muWrite is held cannot deadlock.
	s.reconcileRemote(saved.RemoteMcpSettings)

	// The remote-gateway fields are the ones whose effects outlive this RPC (the
	// tunnel supervisor reacts to them), so call them out; the manager logs the
	// resulting connection transitions itself.
	flog.Info("config updated", "remoteMcp", saved.RemoteMcpSettings.Enabled, "remoteFleet", saved.RemoteMcpSettings.FleetEnabled, "webhook", saved.RemoteMcpSettings.WebhookEnabled, "gateway", saved.RemoteMcpSettings.GatewayURL)

	return &fleetgrpc.SetConfigReply{Config: protoconv.ConfigToProto(saved)}, nil
}
