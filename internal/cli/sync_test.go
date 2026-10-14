package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// fakeSync is a daemon's sync RPCs.
type fakeSync struct {
	fleetgrpc.FleetServiceClient
	status    *fleetgrpc.GetShareSyncReply
	wait      *fleetgrpc.WaitShareSyncReply
	conflicts *fleetgrpc.ListShareConflictsReply
	waitReq   *fleetgrpc.WaitShareSyncRequest
	getReq    *fleetgrpc.GetShareSyncRequest
}

func (f *fakeSync) GetShareSync(_ context.Context, req *fleetgrpc.GetShareSyncRequest, _ ...grpc.CallOption) (*fleetgrpc.GetShareSyncReply, error) {
	f.getReq = req
	return f.status, nil
}

func (f *fakeSync) WaitShareSync(_ context.Context, req *fleetgrpc.WaitShareSyncRequest, _ ...grpc.CallOption) (*fleetgrpc.WaitShareSyncReply, error) {
	f.waitReq = req
	return f.wait, nil
}

func (f *fakeSync) ListShareConflicts(_ context.Context, _ *fleetgrpc.ListShareConflictsRequest, _ ...grpc.CallOption) (*fleetgrpc.ListShareConflictsReply, error) {
	return f.conflicts, nil
}

func runSyncCLI(t *testing.T, f *fakeSync, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	orig := dialCluster
	t.Cleanup(func() { dialCluster = orig })
	dialCluster = func(context.Context) (fleetgrpc.FleetServiceClient, func(), error) { return f, func() {}, nil }
	cmd := NewRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestSyncStatus(t *testing.T) {
	f := &fakeSync{status: &fleetgrpc.GetShareSyncReply{Fleets: []*fleetgrpc.FleetShareSync{{
		Fleet: "alpha", Paused: []string{"share full"}, Conflicts: 2,
		Shares: []*fleetgrpc.KubeShareUsage{{Cluster: "c1", Location: "prod-eu", Claim: "fs-alpha-1a2b3c4d-share", UsedBytes: 9 << 30, CapacityBytes: 10 << 30, Measured: true, Warning: "90% full; auto-expand is off on prod-eu"}},
		Roots: []*fleetgrpc.ShareSyncRoot{
			{Root: ".claude", Files: 12, Bytes: 4096, Cap: 8 << 30, Sides: []*fleetgrpc.ShareSyncSide{{Side: "host"}, {Side: "k8s:c1/fs-alpha-1a2b3c4d-share"}}},
			{Root: ".mnt", Files: 1, Bytes: 6 << 20, Sides: []*fleetgrpc.ShareSyncSide{{Side: "k8s:c1/fs-alpha-1a2b3c4d-share", Pending: 1, Paused: "share full"}}},
		},
	}}}}
	out, _, err := runSyncCLI(t, f, "sync", "status", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if f.getReq.GetFleet() != "alpha" {
		t.Errorf("asked for %q", f.getReq.GetFleet())
	}
	for _, want := range []string{
		"alpha: sync paused: share full\n",
		"  share fs-alpha-1a2b3c4d-share on prod-eu: 9.0 GiB of 10.0 GiB (90%); 90% full; auto-expand is off on prod-eu\n",
		".claude  12     4.0 KiB  8.0 GiB  in sync",
		".mnt     1      6.0 MiB  -        k8s:c1/fs-alpha-1a2b3c4d-share paused: share full (1 to send)",
		"  2 conflict copies kept (fleet sync conflicts alpha)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	f.status = &fleetgrpc.GetShareSyncReply{}
	out, _, _ = runSyncCLI(t, f, "sync", "status")
	if !strings.Contains(out, "Nothing to sync") {
		t.Errorf("status with nothing to sync:\n%s", out)
	}
}

func TestSyncWait(t *testing.T) {
	f := &fakeSync{wait: &fleetgrpc.WaitShareSyncReply{Synced: true}}
	if _, _, err := runSyncCLI(t, f, "sync", "wait", "alpha", "--timeout", "30s"); err != nil {
		t.Fatalf("a synced wait: %v", err)
	}
	if f.waitReq.GetFleet() != "alpha" || f.waitReq.GetTimeoutMs() != 30000 {
		t.Errorf("wait request %v", f.waitReq)
	}
	f.wait = &fleetgrpc.WaitShareSyncReply{Pending: []string{"host .claude: 3 to send"}}
	_, stderr, err := runSyncCLI(t, f, "sync", "wait")
	if err == nil || !strings.Contains(err.Error(), "not in sync within 2m0s") || !strings.Contains(stderr, "  host .claude: 3 to send\n") {
		t.Errorf("a wait that timed out: %v\n%s", err, stderr)
	}
}

func TestSyncConflicts(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	f := &fakeSync{conflicts: &fleetgrpc.ListShareConflictsReply{Conflicts: []*fleetgrpc.ShareConflict{{
		Id: "c1", Time: timestamppb.New(at), Fleet: "alpha", Root: ".mnt", Path: "opt/data/notes.md", Side: "host", Reason: "a newer change won", File: "/home/me/.fleet/sync/alpha/conflicts/c1.data",
	}}}}
	out, _, err := runSyncCLI(t, f, "sync", "conflicts")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TIME", "alpha  .mnt/opt/data/notes.md  host  a newer change won  /home/me/.fleet/sync/alpha/conflicts/c1.data"} {
		if !strings.Contains(out, want) {
			t.Errorf("conflicts lacks %q:\n%s", want, out)
		}
	}
	f.conflicts = &fleetgrpc.ListShareConflictsReply{}
	if out, _, _ := runSyncCLI(t, f, "sync", "conflicts", "alpha"); !strings.Contains(out, "No conflict copies.") {
		t.Errorf("no conflicts:\n%s", out)
	}
}
