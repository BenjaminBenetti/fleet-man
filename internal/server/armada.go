package server

import (
	"context"
	"errors"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// armada.go implements the fleet-armada registry RPCs (armada.json). The
// registry is client-side data — the list of remote fleets THIS machine's user
// can switch to — so the TUI always sends these to the LOCAL daemon, even
// while its main connection points at a remote fleet. The handlers themselves
// can't tell (and don't care) which transport they arrived on; locality is the
// client's responsibility (fleetclient.DialLocal).

// GetArmada returns the registered remote fleets (empty when armada.json is
// absent).
func (s *service) GetArmada(_ context.Context, _ *fleetgrpc.GetArmadaRequest) (*fleetgrpc.GetArmadaReply, error) {
	a, err := state.LoadArmada()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load armada: %v", err)
	}
	return &fleetgrpc.GetArmadaReply{Remotes: armadaToProto(a)}, nil
}

// SetArmada replaces the whole registry (the settings page sends the full
// edited list). muWrite serializes it alongside config.json writes — both are
// small whole-file replaces owned by this server.
func (s *service) SetArmada(ctx context.Context, req *fleetgrpc.SetArmadaRequest) (*fleetgrpc.SetArmadaReply, error) {
	next := protoToArmada(req.GetRemotes())
	// Looked up before taking muWrite: `ssh -G` runs the user's Match exec
	// commands, which may take as long as they like, and every config write
	// waits on muWrite. The registry read here is only a first guess at which
	// entries are new; inheritForwardAgent decides under the lock.
	var resolved map[string]bool
	if early, err := state.LoadArmada(); err == nil {
		resolved = resolveForwardAgent(ctx, early, next)
	}

	s.muWrite.Lock()
	defer s.muWrite.Unlock()

	prev, err := state.LoadArmada()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load armada: %v", err)
	}
	inheritForwardAgent(ctx, prev, next, resolved)
	if err := state.SaveArmada(next); err != nil {
		return nil, status.Errorf(codes.Internal, "save armada: %v", err)
	}
	saved, err := state.LoadArmada()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload armada: %v", err)
	}

	// Never log URLs-with-tokens or tokens; the count is the useful signal.
	flog.Info("armada updated", "remotes", len(saved.Remotes))

	// Tear down the ssh forwards of remotes this edit REMOVED — and only those.
	// An unregistered tunnel is not an orphan: a session booted with FLEET_SSH
	// rides one, and pruning "everything not in the registry" would cut it on
	// any unrelated edit. nil in newService() tests.
	if s.sshTunnels != nil {
		s.sshTunnels.Remove(droppedSSHRemotes(prev, saved))
	}

	return &fleetgrpc.SetArmadaReply{Remotes: armadaToProto(saved)}, nil
}

// sshForwardAgentConfigured reports the user's ssh-config ForwardAgent for an
// ssh:// URL. A var so tests do not depend on the machine's ~/.ssh/config.
var sshForwardAgentConfigured = sshtunnel.ForwardAgentConfigured

// inheritForwardAgent turns agent forwarding on for ssh:// remotes that this
// edit ADDS when the user's ssh config already forwards an agent to that host:
// registering a host you `ssh -A` into should behave the same way. Only new
// entries are touched, so turning it off afterwards sticks. resolved holds the
// answers resolveForwardAgent looked up ahead (keyed by sshtunnel.Key); an
// entry it lacks — the registry changed in between — is looked up here.
func inheritForwardAgent(ctx context.Context, prev, next *state.Armada, resolved map[string]bool) {
	for _, i := range addedSSHRemotes(prev, next) {
		r := next.Remotes[i]
		configured, ok := resolved[sshtunnel.Key(r.URL)]
		if !ok {
			configured = sshForwardAgentConfigured(ctx, r.URL)
		}
		next.Remotes[i].ForwardAgent = configured
	}
}

// resolveForwardAgent looks up sshForwardAgentConfigured for the entries
// inheritForwardAgent would touch, without changing next.
func resolveForwardAgent(ctx context.Context, prev, next *state.Armada) map[string]bool {
	resolved := make(map[string]bool)
	for _, i := range addedSSHRemotes(prev, next) {
		r := next.Remotes[i]
		resolved[sshtunnel.Key(r.URL)] = sshForwardAgentConfigured(ctx, r.URL)
	}
	return resolved
}

// addedSSHRemotes indexes the ssh:// entries of next that prev does not have
// (compared canonically) and that do not already forward the agent.
func addedSSHRemotes(prev, next *state.Armada) []int {
	known := make(map[string]bool)
	for _, r := range prev.Remotes {
		if key := sshtunnel.Key(r.URL); key != "" {
			known[key] = true
		}
	}
	var added []int
	for i, r := range next.Remotes {
		if key := sshtunnel.Key(r.URL); key != "" && !known[key] && !r.ForwardAgent {
			added = append(added, i)
		}
	}
	return added
}

// droppedSSHRemotes lists the ssh:// URLs present in prev but not in next
// (compared canonically, so a respelling of the same remote is not a drop).
func droppedSSHRemotes(prev, next *state.Armada) []string {
	kept := make(map[string]bool)
	for _, r := range next.Remotes {
		if key := sshtunnel.Key(r.URL); key != "" {
			kept[key] = true
		}
	}
	var dropped []string
	for _, r := range prev.Remotes {
		if key := sshtunnel.Key(r.URL); key != "" && !kept[key] {
			dropped = append(dropped, r.URL)
		}
	}
	return dropped
}

// ResolveArmadaRemote brings up (or reuses) the local ssh forward for an ssh://
// remote and returns the loopback address + bearer token a client dials (see
// internal/server/sshtunnel). LOCAL-ONLY (remote_auth.go): it returns a
// credential and runs the user's ssh. Failures are FailedPrecondition with a
// user-facing message — the settings page shows it verbatim as the remote's
// status — except a malformed URL, which is InvalidArgument.
func (s *service) ResolveArmadaRemote(ctx context.Context, req *fleetgrpc.ResolveArmadaRemoteRequest) (*fleetgrpc.ResolveArmadaRemoteReply, error) {
	if !sshtunnel.IsSSHURL(req.GetUrl()) {
		return nil, status.Errorf(codes.InvalidArgument, "not an ssh:// remote: %q", req.GetUrl())
	}
	if _, err := sshtunnel.ParseURL(req.GetUrl()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if s.sshTunnels == nil {
		return nil, status.Error(codes.Unavailable, "ssh tunnels are not available on this daemon")
	}
	ep, err := s.sshTunnels.Resolve(ctx, req.GetUrl())
	if err != nil {
		return nil, sshResolveStatus(req.GetUrl(), err)
	}
	return &fleetgrpc.ResolveArmadaRemoteReply{Addr: ep.Addr, Token: ep.Token}, nil
}

// TrustSSHHostKey appends one host-key line the daemon previously offered for
// an ssh:// remote (an UnknownSSHHostKey detail) to the daemon user's
// known_hosts, then resolves the remote again. LOCAL-ONLY (remote_auth.go):
// it writes the user's known_hosts and hands out a bearer token. The daemon
// only ever writes a line it fetched itself (sshtunnel.Manager.TrustHostKey
// refuses anything else), so a client cannot inject arbitrary entries.
func (s *service) TrustSSHHostKey(ctx context.Context, req *fleetgrpc.TrustSSHHostKeyRequest) (*fleetgrpc.TrustSSHHostKeyReply, error) {
	if !sshtunnel.IsSSHURL(req.GetUrl()) {
		return nil, status.Errorf(codes.InvalidArgument, "not an ssh:// remote: %q", req.GetUrl())
	}
	if strings.TrimSpace(req.GetKnownHostsLine()) == "" {
		return nil, status.Error(codes.InvalidArgument, "known_hosts_line is required")
	}
	if s.sshTunnels == nil {
		return nil, status.Error(codes.Unavailable, "ssh tunnels are not available on this daemon")
	}
	ep, err := s.sshTunnels.TrustHostKey(ctx, req.GetUrl(), req.GetKnownHostsLine())
	if err != nil {
		if errors.Is(err, sshtunnel.ErrKeyNotOffered) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, sshResolveStatus(req.GetUrl(), err)
	}
	return &fleetgrpc.TrustSSHHostKeyReply{Addr: ep.Addr, Token: ep.Token}, nil
}

// sshResolveStatus maps a tunnel bring-up failure to its gRPC status: always
// FailedPrecondition with the user-facing reason, and — for an unknown host
// key — an UnknownSSHHostKey detail carrying the fingerprints and known_hosts
// lines the client needs to ask the user. A changed key is a plain error (its
// message names the offending known_hosts line); it is never offered.
func sshResolveStatus(url string, err error) error {
	st := status.New(codes.FailedPrecondition, err.Error())
	var uk *sshtunnel.UnknownHostKeyError
	if errors.As(err, &uk) {
		detail := &fleetgrpc.UnknownSSHHostKey{
			Url:            url,
			Name:           uk.Name,
			Host:           uk.Host,
			Port:           uint32(uk.Port),
			KeyType:        uk.KeyType,
			KnownHostsPath: uk.KnownHostsPath,
		}
		for _, k := range uk.Keys {
			detail.Keys = append(detail.Keys, &fleetgrpc.SSHHostKey{KeyType: k.Type, Fingerprint: k.Fingerprint, KnownHostsLine: k.Line})
		}
		if withDetail, derr := st.WithDetails(detail); derr == nil {
			st = withDetail
		}
	}
	return st.Err()
}

func armadaToProto(a *state.Armada) []*fleetgrpc.ArmadaRemote {
	if a == nil {
		return nil
	}
	return protoconv.ArmadaRemotesToProto(a.Remotes)
}

func protoToArmada(remotes []*fleetgrpc.ArmadaRemote) *state.Armada {
	return &state.Armada{Remotes: protoconv.ArmadaRemotesFromProto(remotes)}
}
