package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubCopyOutExec runs the CopyFile execs (the single-file tar and the folder
// tar) on the host with the instance's WorkspaceDir as the cwd, as
// stubCopyIntoExec does for the IN direction: a temp dir stands in for the
// container filesystem.
func stubCopyOutExec(t *testing.T) {
	t.Helper()
	hostExec := func(inst *fleet.Instance, argv []string) *exec.Cmd {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = inst.WorkspaceDir
		return cmd
	}
	origFile, origFolder := fileOutCommand, folderExecCommand
	fileOutCommand = func(inst *fleet.Instance, argv []string) *backend.Cmd {
		return backend.NewCmd(hostExec(inst, argv), nil)
	}
	folderExecCommand = hostExec
	t.Cleanup(func() { fileOutCommand, folderExecCommand = origFile, origFolder })
}

// seedCopyFleet seeds fleet alpha with running instances i1 and i2 (their
// workspaces returned) and a stopped one, off.
func seedCopyFleet(t *testing.T) (ws1, ws2 string) {
	t.Helper()
	ws1, ws2 = t.TempDir(), t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendDevcontainer, WorkspaceDir: ws1, ContainerID: "c1", Status: fleet.StatusRunning},
			{Name: "i2", Backend: fleet.BackendDevcontainer, WorkspaceDir: ws2, ContainerID: "c2", Status: fleet.StatusRunning},
			{Name: "off", Backend: fleet.BackendDevcontainer, WorkspaceDir: t.TempDir(), ContainerID: "c3", Status: fleet.StatusStopped},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return ws1, ws2
}

// mcpCopyHarness isolates the fleet dir, seeds the fleet, stubs every copy
// exec onto the host and connects an MCP client.
func mcpCopyHarness(t *testing.T) (cs *mcp.ClientSession, ws1, ws2 string) {
	t.Helper()
	isolateFleetDir(t)
	ws1, ws2 = seedCopyFleet(t)
	stubCopyIntoExec(t)
	stubCopyFileInto(t)
	stubCopyOutExec(t)
	return mcpConnect(t, newService()), ws1, ws2
}

// randomPayload spans several 64 KB data frames, so a frame whose buffer the
// sender reuses before the receiver wrote it out would corrupt the copy.
func randomPayload(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 5*copyChunkSize+123)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireSameFile(t *testing.T, p string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes differ from the %d-byte source", p, len(got), len(want))
	}
}

func TestMCPCopyInstanceToHost(t *testing.T) {
	cs, ws1, _ := mcpCopyHarness(t)
	payload := randomPayload(t)
	writeFile(t, filepath.Join(ws1, "out", "report.bin"), payload)
	hostDir := t.TempDir()

	var out FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"fleet": "alpha", "instance": "i1", "path": "out/report.bin"},
		"dst": map[string]any{"path": hostDir + "/"},
	}, &out)

	want := filepath.Join(hostDir, "report.bin")
	if out.Path != want || out.Bytes != int64(len(payload)) {
		t.Fatalf("got %+v, want path=%s bytes=%d", out, want, len(payload))
	}
	requireSameFile(t, want, payload)
}

func TestMCPCopyHostToInstance(t *testing.T) {
	cs, ws1, _ := mcpCopyHarness(t)
	payload := randomPayload(t)
	src := filepath.Join(t.TempDir(), "tool.bin")
	writeFile(t, src, payload)
	if err := os.Mkdir(filepath.Join(ws1, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"path": src},
		"dst": map[string]any{"fleet": "alpha", "instance": "i1", "path": "sub"},
	}, &out)

	want := filepath.Join(ws1, "sub", "tool.bin")
	if out.Path != want || out.Bytes != int64(len(payload)) {
		t.Fatalf("got %+v, want path=%s bytes=%d", out, want, len(payload))
	}
	requireSameFile(t, want, payload)
}

func TestMCPCopyInstanceToInstance(t *testing.T) {
	cs, ws1, ws2 := mcpCopyHarness(t)
	payload := randomPayload(t)
	writeFile(t, filepath.Join(ws1, "a.bin"), payload)

	var out FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"fleet": "alpha", "instance": "i1", "path": "a.bin"},
		"dst": map[string]any{"fleet": "alpha", "instance": "i2", "path": "b.bin"},
	}, &out)

	want := filepath.Join(ws2, "b.bin")
	if out.Path != want {
		t.Fatalf("path = %q, want %s", out.Path, want)
	}
	requireSameFile(t, want, payload)
}

// TestMCPCopyDirectoryRoundTrip uploads a host tree into an instance and
// downloads it back out to the host.
func TestMCPCopyDirectoryRoundTrip(t *testing.T) {
	cs, ws1, _ := mcpCopyHarness(t)
	payload := randomPayload(t)
	srcDir := filepath.Join(t.TempDir(), "proj")
	writeFile(t, filepath.Join(srcDir, "big.bin"), payload)
	writeFile(t, filepath.Join(srcDir, "sub", "note.txt"), []byte("hi"))

	var up FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"path": srcDir},
		"dst": map[string]any{"fleet": "alpha", "instance": "i1", "path": "."},
	}, &up)
	if up.Path != filepath.Join(ws1, "proj") {
		t.Fatalf("upload path = %q, want %s/proj", up.Path, ws1)
	}
	requireSameFile(t, filepath.Join(ws1, "proj", "big.bin"), payload)

	back := t.TempDir()
	var down FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"fleet": "alpha", "instance": "i1", "path": "proj"},
		"dst": map[string]any{"path": filepath.Join(back, "copy")},
	}, &down)
	if down.Path != filepath.Join(back, "copy") {
		t.Fatalf("download path = %q, want %s/copy", down.Path, back)
	}
	requireSameFile(t, filepath.Join(back, "copy", "big.bin"), payload)
	requireSameFile(t, filepath.Join(back, "copy", "sub", "note.txt"), []byte("hi"))
}

// TestMCPCopyHomeRelativeHostPath resolves a ~/ host path against the daemon
// user's home, keeping the trailing slash's keep-the-name meaning.
func TestMCPCopyHomeRelativeHostPath(t *testing.T) {
	cs, ws1, _ := mcpCopyHarness(t)
	writeFile(t, filepath.Join(ws1, "a.txt"), []byte("hello"))
	home, _ := os.UserHomeDir()
	if err := os.Mkdir(filepath.Join(home, "got"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out FleetCopyOutput
	callJSON(t, cs, "fleet_copy", map[string]any{
		"src": map[string]any{"fleet": "alpha", "instance": "i1", "path": "a.txt"},
		"dst": map[string]any{"path": "~/got/"},
	}, &out)
	if want := filepath.Join(home, "got", "a.txt"); out.Path != want {
		t.Fatalf("path = %q, want %s", out.Path, want)
	}
	requireSameFile(t, out.Path, []byte("hello"))
}

func TestMCPCopyRejects(t *testing.T) {
	cs, ws1, _ := mcpCopyHarness(t)
	writeFile(t, filepath.Join(ws1, "a.txt"), []byte("x"))
	inst := map[string]any{"fleet": "alpha", "instance": "i1", "path": "a.txt"}
	cases := []struct {
		name     string
		src, dst map[string]any
		want     string
	}{
		{"relative host path", inst, map[string]any{"path": "out.txt"}, "must be absolute"},
		{"fleet without instance", inst, map[string]any{"fleet": "alpha", "path": "/tmp/x"}, "both fleet and instance"},
		{"instance without fleet", map[string]any{"instance": "i1", "path": "a.txt"}, map[string]any{"path": "/tmp/x"}, "both fleet and instance"},
		{"empty path", inst, map[string]any{"fleet": "alpha", "instance": "i2", "path": ""}, "dst.path is required"},
		{"stopped instance", inst, map[string]any{"fleet": "alpha", "instance": "off", "path": "."}, "not running"},
		{"unknown instance", map[string]any{"fleet": "alpha", "instance": "ghost", "path": "a.txt"}, map[string]any{"path": "/tmp/x"}, "ghost"},
		{"missing source", map[string]any{"fleet": "alpha", "instance": "i1", "path": "nope.txt"}, map[string]any{"path": t.TempDir() + "/"}, "nope.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "fleet_copy",
				Arguments: map[string]any{"src": tc.src, "dst": tc.dst},
			})
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError || !strings.Contains(toolText(res), tc.want) {
				t.Fatalf("want a tool error containing %q, got isError=%v %q", tc.want, res.IsError, toolText(res))
			}
		})
	}
}

func TestResolveHostCopyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := map[string]string{
		"/abs/file":    "/abs/file",
		"/abs/dir/":    "/abs/dir/",
		"~":            home,
		"~/":           home + "/",
		"~/x/y":        filepath.Join(home, "x", "y"),
		"~/Downloads/": filepath.Join(home, "Downloads") + "/",
	}
	for in, want := range cases {
		got, err := resolveHostCopyPath(in)
		if err != nil || got != want {
			t.Errorf("resolveHostCopyPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"rel", "./rel", "~other/x"} {
		if _, err := resolveHostCopyPath(bad); err == nil {
			t.Errorf("resolveHostCopyPath(%q) succeeded, want an error", bad)
		}
	}
}
