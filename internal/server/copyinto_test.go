package server

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeChunkReceiver replays canned CopyInto chunks for writeCopyIntoTar tests.
type fakeChunkReceiver struct {
	chunks []*fleetgrpc.CopyIntoChunk
	i      int
}

func (f *fakeChunkReceiver) Recv() (*fleetgrpc.CopyIntoChunk, error) {
	if f.i >= len(f.chunks) {
		return nil, io.EOF
	}
	c := f.chunks[f.i]
	f.i++
	return c, nil
}

func dataChunk(b string) *fleetgrpc.CopyIntoChunk {
	return &fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: []byte(b)}}
}

func TestWriteCopyIntoTarRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	rcv := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("hello "), dataChunk("world")}}
	if err := writeCopyIntoTar(&buf, rcv, "tool", 0o755, 11); err != nil {
		t.Fatalf("writeCopyIntoTar: %v", err)
	}
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar Next: %v", err)
	}
	if hdr.Name != "tool" || hdr.Mode != 0o755 || hdr.Size != 11 {
		t.Fatalf("header = name=%q mode=%o size=%d, want tool 0755 11", hdr.Name, hdr.Mode, hdr.Size)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "hello world" {
		t.Fatalf("body = %q, want hello world", body)
	}
}

func TestWriteCopyIntoTarSizeMismatch(t *testing.T) {
	// Stream ends short of the declared size.
	short := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("hi")}}
	if err := writeCopyIntoTar(io.Discard, short, "f", 0o644, 10); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("short stream: want InvalidArgument, got %v", err)
	}
	// Stream overruns the declared size.
	over := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("too much data")}}
	if err := writeCopyIntoTar(io.Discard, over, "f", 0o644, 4); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over stream: want InvalidArgument, got %v", err)
	}
}

func TestValidateCopyName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "/abs", "sub/file"} {
		if err := validateCopyName(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("validateCopyName(%q): want InvalidArgument, got %v", bad, err)
		}
	}
	for _, ok := range []string{"tool", "out.bin", "a.tar.gz"} {
		if err := validateCopyName(ok); err != nil {
			t.Errorf("validateCopyName(%q): unexpected error %v", ok, err)
		}
	}
}

// stubCopyIntoExec runs the CopyInto exec (test -d, tar -xf -) on the host with
// the command's cwd set to the instance's WorkspaceDir — so a real temp dir
// stands in for the container filesystem, no backend required.
func stubCopyIntoExec(t *testing.T) {
	t.Helper()
	orig := copyIntoExecCommand
	copyIntoExecCommand = func(inst *fleet.Instance, argv []string) *exec.Cmd {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = inst.WorkspaceDir
		return cmd
	}
	t.Cleanup(func() { copyIntoExecCommand = orig })
}

func seedCopyIntoInstance(t *testing.T, ws string) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendDevcontainer, WorkspaceDir: ws, ContainerID: "c1", Status: fleet.StatusRunning},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func sendCopyInto(t *testing.T, stream fleetgrpc.FleetService_CopyIntoClient, open *fleetgrpc.CopyIntoOpen, body string) (*fleetgrpc.CopyIntoReply, error) {
	t.Helper()
	if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Open{Open: open}}); err != nil {
		return nil, err
	}
	if body != "" {
		if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: []byte(body)}}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// TestCopyIntoWritesFileToDir exercises the full handler over a real bufconn,
// with the exec stubbed onto the host: a file streamed into an existing
// directory lands at dir/name with the requested mode.
func TestCopyIntoWritesFileToDir(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	dst := filepath.Join(ws, "sub")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	seedCopyIntoInstance(t, ws)
	stubCopyIntoExec(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	reply, err := sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Dest: dst, Name: "tool", Mode: 0o755, Size: 5}, "hello")
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	want := filepath.Join(dst, "tool")
	if reply.GetPath() != want || reply.GetWritten() != 5 {
		t.Fatalf("reply = %v, want path=%s written=5", reply, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("written %q, want hello", got)
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755", fi.Mode().Perm())
	}
}

// TestCopyIntoEmptyDestUsesWorkspace confirms an empty dest extracts into the
// workspace folder (the exec cwd) keeping the source name.
func TestCopyIntoEmptyDestUsesWorkspace(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	seedCopyIntoInstance(t, ws)
	stubCopyIntoExec(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	reply, err := sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Dest: "", Name: "out.bin", Mode: 0o644, Size: 3}, "abc")
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "out.bin")); err != nil || string(got) != "abc" {
		t.Fatalf("workspace file = %q (err %v), want abc", got, err)
	}
	if reply.GetWritten() != 3 {
		t.Fatalf("written = %d, want 3", reply.GetWritten())
	}
}

// TestCopyIntoSizeMismatchPreservesExisting is the atomicity guarantee: a
// truncated upload (declared size > bytes streamed) fails the RPC and leaves a
// pre-existing destination file untouched, with no temp file left behind.
func TestCopyIntoSizeMismatchPreservesExisting(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	dst := filepath.Join(ws, "sub")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dst, "tool")
	if err := os.WriteFile(existing, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedCopyIntoInstance(t, ws)
	stubCopyIntoExec(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	// Declare 10 bytes but stream only 3 — a short upload.
	_, err = sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Dest: dst, Name: "tool", Mode: 0o644, Size: 10}, "abc")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("short upload: want InvalidArgument, got %v", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "ORIGINAL" {
		t.Fatalf("existing file clobbered: %q, want ORIGINAL", got)
	}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fleetcopy-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// TestCopyIntoMissingDirIsNotFound confirms a non-existent destination directory
// is reported cleanly (before any tar extraction).
func TestCopyIntoMissingDirIsNotFound(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	seedCopyIntoInstance(t, ws)
	stubCopyIntoExec(t)

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	_, err = sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Dest: filepath.Join(ws, "nope") + "/", Name: "x", Size: 1}, "y")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing dir: want NotFound, got %v", err)
	}
}

// TestCopyIntoUnknownInstance fails fast with NotFound before any exec.
func TestCopyIntoUnknownInstance(t *testing.T) {
	isolateFleetDir(t)
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	_, err = sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "ghost", Instance: "x", Dest: "/tmp", Name: "f", Size: 1}, "z")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown instance: want NotFound, got %v", err)
	}
}
