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

func TestDrainCopyIntoToHostFileRoundTrip(t *testing.T) {
	rcv := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("hello "), dataChunk("world")}}
	f, written, err := drainCopyIntoToHostFile(rcv, 11)
	if err != nil {
		t.Fatalf("drainCopyIntoToHostFile: %v", err)
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if written != 11 {
		t.Fatalf("written = %d, want 11", written)
	}
	body, _ := io.ReadAll(f)
	if string(body) != "hello world" {
		t.Fatalf("body = %q, want hello world", body)
	}
}

func TestDrainCopyIntoToHostFileSizeMismatch(t *testing.T) {
	// Stream ends short of the declared size.
	short := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("hi")}}
	if _, _, err := drainCopyIntoToHostFile(short, 10); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("short stream: want InvalidArgument, got %v", err)
	}
	// Stream overruns the declared size.
	over := &fakeChunkReceiver{chunks: []*fleetgrpc.CopyIntoChunk{dataChunk("too much data")}}
	if _, _, err := drainCopyIntoToHostFile(over, 4); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("over stream: want InvalidArgument, got %v", err)
	}
}

func TestValidateCopyName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "/abs", "sub/file", "a\nb", "a\rb", "a\x00b"} {
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

// stubCopyFileInto stands in for the backend's CopyFile: it writes the streamed
// bytes to the resolved path on the host (relative paths resolve against the
// instance's WorkspaceDir, the exec cwd), so a real temp dir models the
// container filesystem without a backend.
func stubCopyFileInto(t *testing.T) {
	t.Helper()
	orig := copyFileInto
	copyFileInto = func(inst *fleet.Instance, src io.Reader, remotePath string, mode int) error {
		p := remotePath
		if !filepath.IsAbs(p) {
			p = filepath.Join(inst.WorkspaceDir, remotePath)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		data, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, data, os.FileMode(mode)); err != nil {
			return err
		}
		return os.Chmod(p, os.FileMode(mode))
	}
	t.Cleanup(func() { copyFileInto = orig })
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
	// gRPC client-streaming idiom: when the server rejects an early message and
	// closes the stream, an in-flight Send returns io.EOF rather than the RPC
	// status. Don't treat that EOF as the result — fall through to CloseAndRecv,
	// which surfaces the real server status (e.g. NotFound). Bailing on the EOF
	// here races the server's rejection and flakes (want NotFound, got EOF).
	if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Open{Open: open}}); err != nil && err != io.EOF {
		return nil, err
	}
	if body != "" {
		if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: []byte(body)}}); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// seedCodespacesInstance seeds a running codespaces instance (the backend whose
// exec PTY makes directory copy unsafe, so the server refuses it).
func seedCodespacesInstance(t *testing.T, ws string) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendCodespaces, WorkspaceDir: ws, ContainerID: "c1", Status: fleet.StatusRunning},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestCopyIntoDirExtracts exercises the full dir IN handler over a real bufconn
// with the exec stubbed onto the host: a tar of contents lands as a tree under
// the resolved target directory.
func TestCopyIntoDirExtracts(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	seedCopyIntoInstance(t, ws)
	stubCopyIntoExec(t)

	body := tarArchive(t,
		&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		&tar.Header{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "sub/b.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
	)

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	// dest empty → the tree lands at <workspace>/proj.
	if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Open{Open: &fleetgrpc.CopyIntoOpen{
		Fleet: "alpha", Instance: "i1", Name: "proj", IsDir: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: body.Bytes()}}); err != nil {
		t.Fatal(err)
	}
	reply, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if reply.GetPath() != filepath.Join(ws, "proj") {
		t.Errorf("reply path = %q, want %s/proj", reply.GetPath(), ws)
	}
	for _, rel := range []string{"proj/a.txt", "proj/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(ws, rel)); err != nil {
			t.Errorf("expected %s extracted: %v", rel, err)
		}
	}
}

// TestCopyIntoDirCodespacesGate confirms the IN dir path is refused on codespaces.
func TestCopyIntoDirCodespacesGate(t *testing.T) {
	isolateFleetDir(t)
	seedCodespacesInstance(t, t.TempDir())

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	_, err = sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Name: "proj", IsDir: true}, "")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("codespaces dir IN: want FailedPrecondition, got %v", err)
	}
}

// TestCopyIntoDirCoderGate confirms the IN dir path is refused on coder rather
// than hanging on its stdin-EOF-bound `tar -xf -`.
func TestCopyIntoDirCoderGate(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendCoder, WorkspaceDir: t.TempDir(), ContainerID: "ws.agent", Status: fleet.StatusRunning},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	stream, err := client.CopyInto(context.Background())
	if err != nil {
		t.Fatalf("CopyInto: %v", err)
	}
	_, err = sendCopyInto(t, stream,
		&fleetgrpc.CopyIntoOpen{Fleet: "alpha", Instance: "i1", Name: "proj", IsDir: true}, "")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("coder dir IN: want FailedPrecondition, got %v", err)
	}
}

// TestCopyFileDirCodespacesGate confirms the OUT dir path is refused on
// codespaces (reached once the `test -d` probe — stubbed onto the host — sees a
// real directory).
func TestCopyFileDirCodespacesGate(t *testing.T) {
	isolateFleetDir(t)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedCodespacesInstance(t, ws)
	stubCopyIntoExec(t) // the dir probe uses copyIntoExecCommand

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	stream, err := client.CopyFile(context.Background(), &fleetgrpc.CopyFileRequest{Fleet: "alpha", Instance: "i1", Path: filepath.Join(ws, "adir")})
	if err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("codespaces dir OUT: want FailedPrecondition, got %v", err)
	}
}

// TestStreamRawTar confirms the raw-tar OUT streamer chunks its input verbatim.
func TestStreamRawTar(t *testing.T) {
	in := bytes.Repeat([]byte("z"), copyChunkSize+100)
	var got []byte
	err := streamRawTar(bytes.NewReader(in), func(c *fleetgrpc.CopyFileChunk) error {
		got = append(got, c.GetData()...)
		return nil
	})
	if err != nil {
		t.Fatalf("streamRawTar: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Errorf("streamRawTar truncated/altered the stream (%d vs %d bytes)", len(got), len(in))
	}
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
	stubCopyFileInto(t)

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
	stubCopyFileInto(t)

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
