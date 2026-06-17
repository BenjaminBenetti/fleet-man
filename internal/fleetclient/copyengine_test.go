package fleetclient

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// contentsTar builds a tar of dir's contents (the wire format for a dir copy).
func contentsTar(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if _, err := writeTarTreeFromWalk(tw, dir); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedTree writes a small fixture tree under dir and returns it.
func seedTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub/b.txt"), []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCopyUploadDirToInstance(t *testing.T) {
	srv := newFakeCopyServer()
	client := dialFake(t, srv)

	src := t.TempDir()
	seedTree(t, src)

	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Local: true, Path: src},
		ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/dst"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.Written != 7 { // "aaa" + "bbbb"
		t.Errorf("written = %d, want 7", res.Written)
	}
	if len(srv.intos) != 1 || !srv.intos[0].isDir {
		t.Fatalf("want one is_dir CopyInto, got %+v", srv.intos)
	}
	// The captured tar must extract back to the same tree.
	out := filepath.Join(t.TempDir(), "extracted")
	if _, err := extractTarTree(bytes.NewReader(srv.intos[0].data), out); err != nil {
		t.Fatalf("extract captured tar: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "sub/b.txt")); string(got) != "bbbb" {
		t.Errorf("round-tripped sub/b.txt = %q, want bbbb", got)
	}
}

func TestCopyDownloadDirToLocal(t *testing.T) {
	srv := newFakeCopyServer()
	src := t.TempDir()
	seedTree(t, src)
	srv.files[fileKey("f", "i", "/proj")] = fakeFile{name: "proj", isDir: true, data: contentsTar(t, src)}
	client := dialFake(t, srv)

	dst := t.TempDir()
	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/proj"},
		ResolvedEndpoint{Local: true, Path: dst + "/"}, // existing dir → dst/proj
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	wantRoot := filepath.Join(dst, "proj")
	if res.DestPath != wantRoot {
		t.Errorf("DestPath = %q, want %q", res.DestPath, wantRoot)
	}
	if got, _ := os.ReadFile(filepath.Join(wantRoot, "a.txt")); string(got) != "aaa" {
		t.Errorf("downloaded a.txt = %q, want aaa", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wantRoot, "sub/b.txt")); string(got) != "bbbb" {
		t.Errorf("downloaded sub/b.txt = %q, want bbbb", got)
	}
}

func TestCopyRelayDirInstanceToInstance(t *testing.T) {
	srv := newFakeCopyServer()
	src := t.TempDir()
	seedTree(t, src)
	srv.files[fileKey("f", "i1", "/proj")] = fakeFile{name: "proj", isDir: true, data: contentsTar(t, src)}
	client := dialFake(t, srv)

	_, err := Copy(context.Background(), client,
		ResolvedEndpoint{Fleet: "f", Instance: "i1", Path: "/proj"},
		ResolvedEndpoint{Fleet: "f", Instance: "i2", Path: "/tmp"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(srv.intos) != 1 || !srv.intos[0].isDir {
		t.Fatalf("relay must propagate is_dir, got %+v", srv.intos)
	}
	// The relayed (filtered) tar still carries the tree.
	out := filepath.Join(t.TempDir(), "relayed")
	if _, err := extractTarTree(bytes.NewReader(srv.intos[0].data), out); err != nil {
		t.Fatalf("extract relayed tar: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "sub/b.txt")); string(got) != "bbbb" {
		t.Errorf("relayed sub/b.txt = %q, want bbbb", got)
	}
}

// fakeCopyServer is an in-memory FleetService exposing just CopyFile + CopyInto,
// so the generic engine can be driven through every direction over the REAL
// generated streaming code without a backend/container. Files are keyed
// "fleet/instance:path"; a CopyInto stores the bytes back as a readable file so
// instance→instance relays round-trip.
type fakeCopyServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
	mu    sync.Mutex
	files map[string]fakeFile
	intos []capturedInto
}

type fakeFile struct {
	name  string
	mode  uint32
	data  []byte
	isDir bool
}

type capturedInto struct {
	fleet, instance, dest, name string
	mode                        uint32
	size                        int64
	data                        []byte
	isDir                       bool
}

func newFakeCopyServer() *fakeCopyServer { return &fakeCopyServer{files: map[string]fakeFile{}} }

func fileKey(fleet, instance, path string) string { return fleet + "/" + instance + ":" + path }

func (f *fakeCopyServer) CopyFile(req *fleetgrpc.CopyFileRequest, stream grpc.ServerStreamingServer[fleetgrpc.CopyFileChunk]) error {
	f.mu.Lock()
	file, ok := f.files[fileKey(req.GetFleet(), req.GetInstance(), req.GetPath())]
	f.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "no file %s", req.GetPath())
	}
	if err := stream.Send(&fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Meta{Meta: &fleetgrpc.CopyFileMeta{
		Name: file.name, Mode: file.mode, Size: int64(len(file.data)), IsDir: file.isDir,
	}}}); err != nil {
		return err
	}
	return stream.Send(&fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Data{Data: file.data}})
}

func (f *fakeCopyServer) CopyInto(stream grpc.ClientStreamingServer[fleetgrpc.CopyIntoChunk, fleetgrpc.CopyIntoReply]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first chunk must be open")
	}
	var data []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		data = append(data, chunk.GetData()...)
	}
	// Resolve a final path the way the real server would for the common cases:
	// a trailing-slash dest (or empty) keeps the source name; else dest is it.
	dest := open.GetDest()
	finalPath := dest
	if dest == "" || strings.HasSuffix(dest, "/") {
		finalPath = strings.TrimRight(dest, "/") + "/" + open.GetName()
	}
	f.mu.Lock()
	f.intos = append(f.intos, capturedInto{
		fleet: open.GetFleet(), instance: open.GetInstance(), dest: dest,
		name: open.GetName(), mode: open.GetMode(), size: open.GetSize(), data: data, isDir: open.GetIsDir(),
	})
	f.files[fileKey(open.GetFleet(), open.GetInstance(), finalPath)] = fakeFile{
		name: open.GetName(), mode: open.GetMode(), data: data,
	}
	f.mu.Unlock()
	return stream.SendAndClose(&fleetgrpc.CopyIntoReply{Path: finalPath, Written: int64(len(data))})
}

// dialFake stands the fake server up over bufconn and returns a client.
func dialFake(t *testing.T, srv *fakeCopyServer) fleetgrpc.FleetServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fleetgrpc.NewFleetServiceClient(conn)
}

// passthroughPolicy treats local paths as absolute as-is, keeping the source
// name for an empty/trailing-slash dest — enough to exercise the engine.
type passthroughPolicy struct{}

func (passthroughPolicy) ResolveSrc(path string) string { return path }
func (passthroughPolicy) ResolveDest(dest, name string) (string, error) {
	if dest == "" {
		return name, nil
	}
	if strings.HasSuffix(dest, "/") {
		return filepath.Join(dest, name), nil
	}
	return dest, nil
}

func TestCopyUploadLocalToInstance(t *testing.T) {
	srv := newFakeCopyServer()
	client := dialFake(t, srv)

	dir := t.TempDir()
	src := filepath.Join(dir, "tool")
	if err := os.WriteFile(src, []byte("hello world"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Local: true, Path: src},
		ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/dst/"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(srv.intos) != 1 {
		t.Fatalf("want 1 CopyInto, got %d", len(srv.intos))
	}
	got := srv.intos[0]
	if got.name != "tool" || string(got.data) != "hello world" || got.size != 11 || got.dest != "/dst/" {
		t.Fatalf("CopyInto open = %+v, want name=tool data=hello world size=11 dest=/dst/", got)
	}
	if got.mode&0o777 != 0o755 {
		t.Fatalf("mode = %o, want 0755", got.mode)
	}
	if res.DestPath != "/dst/tool" || res.Written != 11 {
		t.Fatalf("result = %+v, want /dst/tool 11", res)
	}
}

func TestCopyDownloadInstanceToLocal(t *testing.T) {
	srv := newFakeCopyServer()
	srv.files[fileKey("f", "i", "/bin/tool")] = fakeFile{name: "tool", mode: 0o755, data: []byte("payload")}
	client := dialFake(t, srv)

	dir := t.TempDir()
	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/bin/tool"},
		ResolvedEndpoint{Local: true, Path: dir + "/"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	wantPath := filepath.Join(dir, "tool")
	if res.DestPath != wantPath || res.Written != 7 {
		t.Fatalf("result = %+v, want %s 7", res, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("downloaded %q, want payload", data)
	}
	fi, _ := os.Stat(wantPath)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755", fi.Mode().Perm())
	}
}

// TestCopyDownloadSanitizesServerName ensures a malicious server-provided file
// name cannot steer a directory destination outside it: the name is reduced to a
// basename, so a traversal name lands inside the destination directory.
func TestCopyDownloadSanitizesServerName(t *testing.T) {
	srv := newFakeCopyServer()
	srv.files[fileKey("f", "i", "/evil")] = fakeFile{name: "../../../etc/passwd", mode: 0o644, data: []byte("x")}
	client := dialFake(t, srv)

	dir := t.TempDir()
	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Fleet: "f", Instance: "i", Path: "/evil"},
		ResolvedEndpoint{Local: true, Path: dir + "/"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	want := filepath.Join(dir, "passwd")
	if res.DestPath != want {
		t.Fatalf("traversal name not contained: DestPath = %q, want %q", res.DestPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("file not written inside dest dir: %v", err)
	}
}

func TestCopyRelayInstanceToInstance(t *testing.T) {
	srv := newFakeCopyServer()
	srv.files[fileKey("f", "i1", "/a/out.bin")] = fakeFile{name: "out.bin", mode: 0o644, data: []byte("relayed")}
	client := dialFake(t, srv)

	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Fleet: "f", Instance: "i1", Path: "/a/out.bin"},
		ResolvedEndpoint{Fleet: "f", Instance: "i2", Path: "/tmp/"},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if len(srv.intos) != 1 {
		t.Fatalf("want 1 CopyInto, got %d", len(srv.intos))
	}
	got := srv.intos[0]
	if got.instance != "i2" || got.name != "out.bin" || string(got.data) != "relayed" {
		t.Fatalf("relayed CopyInto = %+v, want i2 out.bin relayed", got)
	}
	if res.DestPath != "/tmp/out.bin" || res.Written != 7 {
		t.Fatalf("result = %+v, want /tmp/out.bin 7", res)
	}
}

func TestCopyLocalToLocal(t *testing.T) {
	srv := newFakeCopyServer()
	client := dialFake(t, srv)

	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(src, []byte("local copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Copy(context.Background(), client,
		ResolvedEndpoint{Local: true, Path: src},
		ResolvedEndpoint{Local: true, Path: dst},
		passthroughPolicy{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if res.DestPath != dst || res.Written != 10 {
		t.Fatalf("result = %+v, want %s 10", res, dst)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "local copy" {
		t.Fatalf("copied %q, want local copy", data)
	}
}
