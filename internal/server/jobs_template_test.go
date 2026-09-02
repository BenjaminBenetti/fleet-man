package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubCreateJob replaces the provisioning seam with one that records the
// remote it was handed and flips the instance to running.
func stubCreateJob(t *testing.T) *string {
	t.Helper()
	var seen string
	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType) error {
		seen = remote
		return state.Update(func(st *state.State) error {
			if f, ok := st.Fleets[fleetName]; ok {
				if inst, err := f.GetInstance(instanceName); err == nil {
					inst.Status = fleet.StatusRunning
					inst.ContainerID = "fake-cid"
				}
			}
			return nil
		})
	}
	t.Cleanup(func() { jobRunCreate = orig })
	return &seen
}

// createErr starts a CreateInstance stream and returns the error its first
// Recv yields (job-start rejections surface there, before any JobStarted).
func createErr(t *testing.T, client fleetgrpc.FleetServiceClient, req *fleetgrpc.CreateInstanceRequest) error {
	t.Helper()
	stream, err := client.CreateInstance(context.Background(), req)
	if err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

func TestCreateInstanceTemplateHappyPathCopiesNotClones(t *testing.T) {
	isolateFleetDir(t)
	seen := stubCreateJob(t)
	tmpl := t.TempDir()
	remote := "file://" + tmpl

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "scratch", Instance: "i1", Remote: &remote,
		Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	evs := drainJob(t, stream)
	last := evs[len(evs)-1].GetDone()
	if last == nil || !last.GetSuccess() {
		t.Fatalf("want successful JobDone, got %v", evs[len(evs)-1])
	}
	if *seen != remote {
		t.Fatalf("provisioner got remote %q, want the template URL %q", *seen, remote)
	}
	st, _ := state.Load()
	if f := st.Fleets["scratch"]; f == nil || f.Remote != remote {
		t.Fatalf("fleet record not created with the template remote: %+v", f)
	}
}

// Rejections must land BEFORE the StatusCreating record exists so `fleet up`
// gets an RPC error instead of a failed instance left behind in state.
func TestCreateInstanceTemplateRejections(t *testing.T) {
	isolateFleetDir(t)
	stubCreateJob(t)
	tmpl := t.TempDir()
	remote := "file://" + tmpl
	missing := "file://" + filepath.Join(t.TempDir(), "nope")
	relative := "file://relative/dir"
	branch := "main"
	notADir := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	notADirRemote := "file://" + notADir

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	cases := []struct {
		name string
		req  *fleetgrpc.CreateInstanceRequest
		code codes.Code
	}{
		{"branch", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &remote, Branch: &branch, Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, codes.InvalidArgument},
		{"coder", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &remote, Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODER}, codes.InvalidArgument},
		{"codespaces", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &remote, Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES}, codes.InvalidArgument},
		{"relative", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &relative, Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, codes.InvalidArgument},
		{"missing dir", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &missing, Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, codes.FailedPrecondition},
		{"not a dir", &fleetgrpc.CreateInstanceRequest{Fleet: "t", Instance: "i", Remote: &notADirRemote, Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, codes.FailedPrecondition},
	}
	for _, tc := range cases {
		err := createErr(t, client, tc.req)
		if status.Code(err) != tc.code {
			t.Errorf("%s: want %v, got %v", tc.name, tc.code, err)
		}
	}

	st, _ := state.Load()
	if f := st.Fleets["t"]; f != nil && len(f.Instances) > 0 {
		t.Fatalf("rejected creates must not leave instance records: %+v", f.Instances)
	}
}

// An existing template fleet enforces the same rules when the remote comes
// from the fleet record rather than the request (the `fleet up` no --repo path).
func TestCreateInstanceTemplateFleetRecordRejectsBranch(t *testing.T) {
	isolateFleetDir(t)
	stubCreateJob(t)
	tmpl := t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"scratch": {Name: "scratch", Remote: "file://" + tmpl},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	branch := "feature/x"
	err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "scratch", Instance: "i1", Branch: &branch,
		Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for branch on template fleet, got %v", err)
	}
	if err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "scratch", Instance: "i1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	}); err != nil {
		t.Fatalf("plain create on template fleet should start: %v", err)
	}
}

func TestInspectRepoTemplateDirInPlace(t *testing.T) {
	isolateFleetDir(t)
	root := fakeRepoDir(t, `{"remoteUser":"vscode"}`)
	svc := newService()
	reply, err := svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "file://" + root})
	if err != nil {
		t.Fatalf("InspectRepo(template): %v", err)
	}
	if !reply.GetHasDevcontainer() {
		t.Fatal("template with devcontainer.json reported as missing")
	}
	// The handler's deferred Close must not have removed the user's dir.
	if _, err := os.Stat(filepath.Join(root, ".devcontainer", "devcontainer.json")); err != nil {
		t.Fatalf("template dir was removed by inspection: %v", err)
	}
	_, err = svc.InspectRepo(context.Background(), &fleetgrpc.InspectRepoRequest{RemoteUrl: "file://" + filepath.Join(t.TempDir(), "nope")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing template dir: want FailedPrecondition, got %v", err)
	}
}
