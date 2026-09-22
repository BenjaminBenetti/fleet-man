package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
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
func (s *service) SetConfig(_ context.Context, req *fleetgrpc.SetConfigRequest) (*fleetgrpc.SetConfigReply, error) {
	s.muWrite.Lock()
	defer s.muWrite.Unlock()

	// Remember whether the microphone was on, to act on it being turned OFF
	// below. An unreadable prior config reads as "was off" — nothing to undo.
	//
	// The previous microphone settings also SEED the otherwise-zero base: a
	// client built before the mic group existed omits it from the Config it
	// sends, and with a zero base that would read as "microphone off" — so
	// changing an unrelated setting from an older TUI would kick every provider
	// and stop every instance's sound server. An absent group means "unchanged";
	// a current client always sends the group, so it still overrides the seed.
	base := &state.Config{}
	micWasEnabled := false
	if previous, err := state.LoadConfig(); err == nil {
		micWasEnabled = previous.MicSettings.Enabled
		base.MicSettings = previous.MicSettings
	}

	if err := state.SaveConfig(protoconv.ConfigFromProto(req.GetConfig(), base)); err != nil {
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

	// Converge the virtual microphone. Turning it ON needs nothing here: the
	// client opens its Mic stream and the hub's sync loop attaches sinks. Turning
	// it OFF is acted on now rather than on the next tick, so the toggle means
	// what it says the moment it is flipped. Non-blocking (the per-instance
	// shutdowns run on their own goroutines).
	if micWasEnabled && !saved.MicSettings.Enabled {
		s.mic.disable()
	} else if saved.MicSettings.Enabled {
		s.mic.setDevice(saved.MicSettings.Device) // pushed to a running provider
		s.mic.poke()
	}

	// The remote-gateway fields are the ones whose effects outlive this RPC (the
	// tunnel supervisor reacts to them), so call them out; the manager logs the
	// resulting connection transitions itself.
	flog.Info("config updated", "remoteMcp", saved.RemoteMcpSettings.Enabled, "remoteFleet", saved.RemoteMcpSettings.FleetEnabled, "webhook", saved.RemoteMcpSettings.WebhookEnabled, "gateway", saved.RemoteMcpSettings.GatewayURL, "mic", saved.MicSettings.Enabled)

	return &fleetgrpc.SetConfigReply{Config: protoconv.ConfigToProto(saved)}, nil
}

// reconcileRemote converges both remote-control transports on the settings:
// the gateway tunnel negotiates grpc only in gateway mode, and the SSH loopback
// listener is up only in SSH mode — so flipping the mode moves the surface
// rather than doubling it. nil-safe for tests that use newService() without a
// serve loop.
func (s *service) reconcileRemote(rm state.RemoteMcpSettings) {
	// A remote client can only provide its SSH agent while remote access is on.
	agentsock.SetRemoteClients(rm.FleetEnabled)
	if s.remote != nil {
		s.remote.Reconcile(rm.Enabled, rm.FleetViaGateway(), rm.WebhookEnabled, rm.GatewayURL)
	}
	if s.sshListen != nil {
		s.sshListen.Reconcile(rm.FleetViaSSH())
	}
}
