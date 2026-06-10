package server

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// tarArchive builds an in-memory tar stream with the given entries, mirroring
// what the in-container `tar -chf -` exec writes to stdout.
func tarArchive(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte{'x'}, int(hdr.Size))); err != nil {
				t.Fatalf("Write(%q): %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return &buf
}

// collectChunks runs streamTarFile and gathers the frames it sends.
func collectChunks(t *testing.T, filePath string, r *bytes.Buffer) ([]*fleetgrpc.CopyFileChunk, error) {
	t.Helper()
	var chunks []*fleetgrpc.CopyFileChunk
	err := streamTarFile(filePath, r, func(c *fleetgrpc.CopyFileChunk) error {
		// Data frames reuse the read buffer between sends; snapshot the bytes
		// the way gRPC's serialization would.
		if d := c.GetData(); d != nil {
			c = &fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Data{Data: bytes.Clone(d)}}
		}
		chunks = append(chunks, c)
		return nil
	})
	return chunks, err
}

func TestStreamTarFileRegularFile(t *testing.T) {
	const size = copyChunkSize + 123 // forces multiple data frames
	buf := tarArchive(t, &tar.Header{Name: "tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: size})

	chunks, err := collectChunks(t, "/workspaces/repo/bin/tool", buf)
	if err != nil {
		t.Fatalf("streamTarFile: %v", err)
	}
	if len(chunks) < 3 { // meta + at least two data frames
		t.Fatalf("want >=3 frames, got %d", len(chunks))
	}
	meta := chunks[0].GetMeta()
	if meta == nil {
		t.Fatal("first frame is not meta")
	}
	if meta.GetName() != "tool" || meta.GetMode() != 0o755 || meta.GetSize() != size {
		t.Fatalf("meta = %v, want name=tool mode=0755 size=%d", meta, size)
	}
	var total int
	for _, c := range chunks[1:] {
		if c.GetMeta() != nil {
			t.Fatal("meta frame after the first")
		}
		total += len(c.GetData())
	}
	if total != size {
		t.Fatalf("data bytes = %d, want %d", total, size)
	}
}

func TestStreamTarFileRejectsDirectory(t *testing.T) {
	buf := tarArchive(t, &tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	_, err := collectChunks(t, "/workspaces/repo/dir", buf)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want InvalidArgument mentioning directory, got %v", err)
	}
}

func TestStreamTarFileRejectsNonRegular(t *testing.T) {
	buf := tarArchive(t, &tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644})
	_, err := collectChunks(t, "/workspaces/repo/fifo", buf)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestStreamTarFileEmptyArchiveIsNotFound(t *testing.T) {
	// tar exits with an error and no entries when the file doesn't exist; the
	// decoder maps that to NotFound (the RPC layer folds in tar's stderr).
	buf := tarArchive(t)
	_, err := collectChunks(t, "/workspaces/repo/ghost", buf)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestCopyFileValidatesRequest(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()

	// Unknown instance fails fast with NotFound, before any backend exec.
	err := svc.CopyFile(&fleetgrpc.CopyFileRequest{Fleet: "ghost", Instance: "x", Path: "/f"}, nopCopyStream{})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for unknown fleet, got %v", err)
	}
}

// nopCopyStream satisfies the CopyFile server-stream interface for request
// validation tests that never reach a Send.
type nopCopyStream struct{}

func (nopCopyStream) Send(*fleetgrpc.CopyFileChunk) error { return nil }
func (nopCopyStream) Context() context.Context            { return context.Background() }
func (nopCopyStream) SetHeader(metadata.MD) error         { return nil }
func (nopCopyStream) SendHeader(metadata.MD) error        { return nil }
func (nopCopyStream) SetTrailer(metadata.MD)              {}
func (nopCopyStream) SendMsg(any) error                   { return nil }
func (nopCopyStream) RecvMsg(any) error                   { return nil }
