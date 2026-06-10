package server

import (
	"archive/tar"
	"bytes"
	"io"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// copy.go implements the CopyFile RPC behind `fleet copy` and the in-instance
// `fc` shorthand: stream one file out of an instance to the client.
//
// The file is read through the backend with a single container exec — `tar` to
// stdout — so the file's metadata (mode, size) and bytes arrive in one
// consistent stream with no shell quoting (argv goes straight to exec) and no
// stat/cat race. -h dereferences a symlinked source so the client receives the
// real content.

// copyChunkSize is the data-frame payload size. Comfortably under gRPC's 4MB
// default message cap while keeping per-frame overhead negligible.
const copyChunkSize = 64 * 1024

// CopyFile streams the file at req.path out of the instance: first a meta
// chunk (name/mode/size), then data chunks until EOF. Only regular files are
// supported. A relative path resolves against the backend exec working
// directory (the workspace folder).
func (s *service) CopyFile(req *fleetgrpc.CopyFileRequest, stream grpc.ServerStreamingServer[fleetgrpc.CopyFileChunk]) error {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return err
	}
	filePath := req.GetPath()
	base := path.Base(filePath)
	if filePath == "" || base == "/" || base == "." || base == ".." {
		return status.Errorf(codes.InvalidArgument, "invalid file path %q", filePath)
	}

	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir,
		[]string{"tar", "-chf", "-", "-C", path.Dir(filePath), base})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start copy: %v", err)
	}

	ctx := stream.Context()
	finished := make(chan struct{})
	// Kill the tar process if the client disconnects mid-stream, so it can't
	// wedge forever writing into a full pipe nobody reads.
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-finished:
		}
	}()

	streamErr := streamTarFile(filePath, stdout, stream.Send)
	if streamErr != nil {
		// The exec's output is useless now; stop it (a no-op if it already
		// exited) before draining and reaping.
		_ = cmd.Process.Kill()
	}
	// Drain the archive trailer so tar isn't killed by a closing pipe, then reap.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	close(finished)

	if streamErr != nil {
		// When the archive never decoded ("no file" rather than "not a file"),
		// tar's own stderr names the cause — "No such file or directory" beats
		// "unexpected EOF". Errors decided by streamTarFile itself (directory,
		// non-regular entry, a dropped client) pass through.
		if status.Code(streamErr) == codes.NotFound {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return status.Errorf(codes.NotFound, "read %q in %s/%s: %s", filePath, req.GetFleet(), req.GetInstance(), msg)
			}
		}
		return streamErr
	}
	if waitErr != nil {
		// tar exited non-zero after a decodable stream (e.g. the file shrank
		// mid-read): the client's copy may be corrupt, so fail the RPC.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return status.Errorf(codes.Internal, "read %q in %s/%s: %s", filePath, req.GetFleet(), req.GetInstance(), msg)
		}
		return status.Errorf(codes.Internal, "read %q in %s/%s: %v", filePath, req.GetFleet(), req.GetInstance(), waitErr)
	}
	return nil
}

// streamTarFile decodes the single-file tar archive on r and forwards it as
// CopyFileChunk frames via send: one meta frame, then data frames. It returns
// a codes.NotFound error when no entry decodes (missing/unreadable file — the
// caller folds in tar's stderr for the real cause), codes.InvalidArgument for
// an entry that is not a regular file, and send errors verbatim.
func streamTarFile(filePath string, r io.Reader, send func(*fleetgrpc.CopyFileChunk) error) error {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil {
		return status.Errorf(codes.NotFound, "read %q: %v", filePath, err)
	}
	if hdr.FileInfo().IsDir() {
		return status.Errorf(codes.InvalidArgument, "%q is a directory — only single files can be copied", filePath)
	}
	if hdr.Typeflag != tar.TypeReg {
		return status.Errorf(codes.InvalidArgument, "%q is not a regular file", filePath)
	}

	meta := &fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Meta{Meta: &fleetgrpc.CopyFileMeta{
		Name: path.Base(filePath),
		Mode: uint32(hdr.FileInfo().Mode().Perm()),
		Size: hdr.Size,
	}}}
	if err := send(meta); err != nil {
		return err
	}

	buf := make([]byte, copyChunkSize)
	for {
		n, readErr := tr.Read(buf)
		if n > 0 {
			chunk := &fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Data{Data: buf[:n]}}
			if err := send(chunk); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read %q: %v", filePath, readErr)
		}
	}
}
