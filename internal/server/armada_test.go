package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestSetArmadaRoundTripsRegistry drives the real SetArmada/GetArmada RPCs and
// verifies the registry survives both the proto round trip and the on-disk
// write (no field loss — the SetConfig round-trip guard's shape).
func TestSetArmadaRoundTripsRegistry(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	in := []*fleetgrpc.ArmadaRemote{
		{Url: "https://gw.example.com/abc", Token: "tok-1"},
		{Url: "http://gw2.example.com:8080/def", Token: "tok-2"},
	}
	setReply, err := svc.SetArmada(ctx, &fleetgrpc.SetArmadaRequest{Remotes: in})
	if err != nil {
		t.Fatalf("SetArmada: %v", err)
	}
	if len(setReply.GetRemotes()) != 2 {
		t.Fatalf("SetArmada reply has %d remotes, want 2", len(setReply.GetRemotes()))
	}

	getReply, err := svc.GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		t.Fatalf("GetArmada: %v", err)
	}
	for i, r := range getReply.GetRemotes() {
		if r.GetUrl() != in[i].GetUrl() || r.GetToken() != in[i].GetToken() {
			t.Fatalf("remote %d mismatch: got %s/%s", i, r.GetUrl(), r.GetToken())
		}
	}

	// Persisted to disk (0600, separate file — not config.json).
	a, err := state.LoadArmada()
	if err != nil {
		t.Fatalf("LoadArmada: %v", err)
	}
	if len(a.Remotes) != 2 || a.Remotes[0].URL != "https://gw.example.com/abc" || a.Remotes[0].Token != "tok-1" {
		t.Fatalf("on-disk registry mismatch: %+v", a.Remotes)
	}
}

// TestGetArmadaEmptyByDefault verifies a fresh daemon reports an empty
// registry rather than an error.
func TestGetArmadaEmptyByDefault(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()

	reply, err := svc.GetArmada(context.Background(), &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		t.Fatalf("GetArmada: %v", err)
	}
	if len(reply.GetRemotes()) != 0 {
		t.Fatalf("expected empty registry, got %v", reply.GetRemotes())
	}
}

// TestSetArmadaReplacesWholeRegistry verifies SetArmada has full-replace
// semantics: a second save with one entry leaves exactly that entry.
func TestSetArmadaReplacesWholeRegistry(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	ctx := context.Background()

	if _, err := svc.SetArmada(ctx, &fleetgrpc.SetArmadaRequest{Remotes: []*fleetgrpc.ArmadaRemote{
		{Url: "https://a.example.com/1", Token: "t1"},
		{Url: "https://b.example.com/2", Token: "t2"},
	}}); err != nil {
		t.Fatalf("SetArmada: %v", err)
	}
	if _, err := svc.SetArmada(ctx, &fleetgrpc.SetArmadaRequest{Remotes: []*fleetgrpc.ArmadaRemote{
		{Url: "https://b.example.com/2", Token: "t2"},
	}}); err != nil {
		t.Fatalf("SetArmada (second): %v", err)
	}

	reply, err := svc.GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		t.Fatalf("GetArmada: %v", err)
	}
	if len(reply.GetRemotes()) != 1 || reply.GetRemotes()[0].GetUrl() != "https://b.example.com/2" {
		t.Fatalf("expected the second save to replace the registry, got %v", reply.GetRemotes())
	}
}

// TestDroppedSSHRemotes pins the SetArmada tunnel-teardown rule: only ssh://
// remotes present before and absent after the edit are dropped (matched
// canonically), never gateway remotes or anything merely unregistered.
func TestDroppedSSHRemotes(t *testing.T) {
	prev := &state.Armada{Remotes: []state.ArmadaRemote{
		{URL: "ssh://a"}, {URL: "ssh://ben@Desktop"}, {URL: "https://gw/x", Token: "t"}, {URL: "ssh://c"},
	}}
	next := &state.Armada{Remotes: []state.ArmadaRemote{
		{URL: "SSH://ben@desktop/"}, {URL: "ssh://c"},
	}}
	got := droppedSSHRemotes(prev, next)
	if len(got) != 1 || got[0] != "ssh://a" {
		t.Fatalf("dropped = %v, want [ssh://a]", got)
	}
	if got := droppedSSHRemotes(next, next); len(got) != 0 {
		t.Fatalf("unchanged registry dropped %v", got)
	}
}

// TestSetArmadaInheritsForwardAgentForNewSSHRemotes: a newly registered ssh://
// remote starts with agent forwarding as the user's ssh config has it for that
// host; an existing entry the user turned off is never flipped back on, and
// gateway remotes are never touched.
func TestSetArmadaInheritsForwardAgentForNewSSHRemotes(t *testing.T) {
	isolateFleetDir(t)
	orig := sshForwardAgentConfigured
	t.Cleanup(func() { sshForwardAgentConfigured = orig })
	asked := map[string]int{}
	sshForwardAgentConfigured = func(_ context.Context, url string) bool {
		asked[url]++
		return url == "ssh://ben@devbox"
	}
	svc := newService()
	ctx := context.Background()

	set := func(remotes ...*fleetgrpc.ArmadaRemote) []*fleetgrpc.ArmadaRemote {
		t.Helper()
		reply, err := svc.SetArmada(ctx, &fleetgrpc.SetArmadaRequest{Remotes: remotes})
		if err != nil {
			t.Fatalf("SetArmada: %v", err)
		}
		return reply.GetRemotes()
	}

	got := set(
		&fleetgrpc.ArmadaRemote{Url: "ssh://ben@devbox"},
		&fleetgrpc.ArmadaRemote{Url: "ssh://ben@other"},
		&fleetgrpc.ArmadaRemote{Url: "https://gw.example.com/abc", Token: "t"},
	)
	if !got[0].GetForwardAgent() || got[1].GetForwardAgent() || got[2].GetForwardAgent() {
		t.Fatalf("new remotes: forward_agent = %v/%v/%v, want true/false/false",
			got[0].GetForwardAgent(), got[1].GetForwardAgent(), got[2].GetForwardAgent())
	}
	if asked["https://gw.example.com/abc"] != 0 {
		t.Fatal("a gateway remote must not be looked up in the ssh config")
	}

	// The user turns it off for devbox: saving again must keep it off.
	got = set(
		&fleetgrpc.ArmadaRemote{Url: "ssh://ben@devbox", ForwardAgent: false},
		&fleetgrpc.ArmadaRemote{Url: "ssh://ben@other", ForwardAgent: true},
	)
	if got[0].GetForwardAgent() || !got[1].GetForwardAgent() {
		t.Fatalf("existing remotes: forward_agent = %v/%v, want the user's false/true", got[0].GetForwardAgent(), got[1].GetForwardAgent())
	}
}
