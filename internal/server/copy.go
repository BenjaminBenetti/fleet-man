package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// copy.go implements the two halves of scp-style `fleet copy` (and the
// in-instance `fc` shorthand): CopyFile streams a file OUT of an instance to the
// client, CopyInto streams a file INTO an instance from the client.
//
// OUT runs `tar -chf -` to stdout (-h dereferences a symlinked source so the
// client receives the real content) and reads the stream until the remote
// process exits — a remote→host close the transport always delivers, so it works
// on every backend.
//
// IN (single file) buffers the upload to a host temp — validating the declared
// size exactly so a truncated/oversized stream fails before anything is written
// — then hands it to the backend's CopyFile, the strategy seam that picks a
// stdin-EOF-safe transport per backend. This is deliberate: a host→remote
// `tar -xf -` reading stdin to EOF hangs forever on the coder backend, whose
// `coder ssh` transport never half-closes the remote command's stdin (issue
// #223); CopyFile streams over stdin on devcontainer/codespaces and transfers
// out-of-band with scp on coder. IN (directory, copyDirInto) still streams a tar
// to `tar -xf -` and so remains subject to that stdin-EOF limitation on coder —
// it is already refused on codespaces (whose exec PTY mangles binary tar) and is
// a tracked follow-up for coder.

// copyChunkSize is the data-frame payload size. Comfortably under gRPC's 4MB
// default message cap while keeping per-frame overhead negligible.
const copyChunkSize = 64 * 1024

// isClientGone reports whether err is an ordinary client disconnect / cancel
// (TUI closed, Ctrl-C mid-copy) rather than a real transfer failure. Such errors
// surface from stream.Send and from a cancelled request context; the repo logs
// these quietly (cf. probeFailureIsAlarming in schedule.go) instead of as
// alarming ERRORs.
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	switch status.Code(err) {
	case codes.Canceled, codes.Unavailable:
		return true
	}
	return false
}

// logCopyOutcome records a copy RPC result once on completion: success at info,
// a benign client cancellation at info ("… canceled", not an alarming ERROR),
// and a genuine failure at error. dir is "out" (CopyFile) or "in" (CopyInto).
func logCopyOutcome(dir, fleetName, instance, p string, start time.Time, err error) {
	switch {
	case err == nil:
		flog.Info("file copied "+dir, "fleet", fleetName, "instance", instance, "path", p, "ms", flog.MillisSince(start))
	case isClientGone(err):
		flog.Info("file copy "+dir+" canceled", "fleet", fleetName, "instance", instance, "path", p, "err", err)
	default:
		flog.Error("file copy "+dir+" failed", "fleet", fleetName, "instance", instance, "path", p, "err", err)
	}
}

// CopyFile streams the file at req.path out of the instance: first a meta
// chunk (name/mode/size), then data chunks until EOF. Only regular files are
// supported. A relative path resolves against the backend exec working
// directory (the workspace folder).
func (s *service) CopyFile(req *fleetgrpc.CopyFileRequest, stream grpc.ServerStreamingServer[fleetgrpc.CopyFileChunk]) (err error) {
	start := time.Now()
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return err
	}
	filePath := req.GetPath()
	defer func() { logCopyOutcome("out", req.GetFleet(), req.GetInstance(), filePath, start, err) }()
	base := path.Base(filePath)
	if filePath == "" || base == "/" || base == "." || base == ".." {
		return status.Errorf(codes.InvalidArgument, "invalid file path %q", filePath)
	}

	// A directory source is a recursive copy: stream a tar of its contents that
	// the client extracts in Go. Everything else falls through to the single-file
	// path below (a missing path also falls through, so tar reports NotFound).
	if s.probeContainerDir(inst, filePath) {
		return s.copyDirOut(inst, req, filePath, base, stream)
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

// copyDirOut streams a tar of the directory at filePath to the client (meta with
// is_dir, then raw tar data). The client extracts it in Go — skipping
// symlinks/special files and the `./` root, sanitizing entry names — so the
// server side stays a plain `tar -C <dir> -cf - .` with no -h (internal symlinks
// are archived as links, never followed). Refused on the codespaces backend,
// whose exec PTY mangles binary tar.
func (s *service) copyDirOut(inst *fleet.Instance, req *fleetgrpc.CopyFileRequest, filePath, base string, stream grpc.ServerStreamingServer[fleetgrpc.CopyFileChunk]) error {
	if inst.Backend == fleet.BackendCodespaces {
		return status.Errorf(codes.FailedPrecondition, "copying a directory is not supported on the codespaces backend")
	}
	if err := stream.Send(&fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Meta{Meta: &fleetgrpc.CopyFileMeta{
		Name:  base,
		IsDir: true,
	}}}); err != nil {
		return err
	}

	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir,
		[]string{"tar", "-C", filePath, "-cf", "-", "."})
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
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-finished:
		}
	}()

	streamErr := streamRawTar(stdout, stream.Send)
	if streamErr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	close(finished)

	if streamErr != nil {
		return streamErr
	}
	if waitErr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return status.Errorf(codes.Internal, "read %q in %s/%s: %s", filePath, req.GetFleet(), req.GetInstance(), msg)
		}
		return status.Errorf(codes.Internal, "read %q in %s/%s: %v", filePath, req.GetFleet(), req.GetInstance(), waitErr)
	}
	return nil
}

// streamRawTar forwards the raw bytes on r as CopyFileChunk data frames — the
// directory case, where the bytes are an opaque tar the client decodes.
func streamRawTar(r io.Reader, send func(*fleetgrpc.CopyFileChunk) error) error {
	buf := make([]byte, copyChunkSize)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := send(&fleetgrpc.CopyFileChunk{Msg: &fleetgrpc.CopyFileChunk_Data{Data: buf[:n]}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read tar: %v", readErr)
		}
	}
}

// copyIntoExecCommand builds the host-side container exec used by CopyInto.
// Package var so tests can stub the whole backend layer (mirrors
// exec_stream.go's buildExecCommand). ExecCommandQuiet because the handler
// drives the command with Start/Wait (not Run/Output), so the wrapper's
// completion log never fires anyway, and a copy already logs nothing per frame.
var copyIntoExecCommand = func(inst *fleet.Instance, argv []string) *exec.Cmd {
	return backendutil.NewForInstance(inst, false).ExecCommandQuiet(inst.WorkspaceDir, argv).Cmd
}

// CopyInto streams the file in the client's chunks INTO the instance. The first
// chunk carries the open header (instance, dest, name, mode, size); the rest
// carry data. The server resolves dest inside the container, buffers the stream
// to a host temp validated against the declared size (a hard cap), then hands it
// to the backend's CopyFile — which writes it atomically (temp + rename), so a
// truncated or oversized stream fails the RPC without ever clobbering an existing
// destination, the same atomicity the download direction has. Only single
// regular files; the mode is preserved. (A directory upload routes to
// copyDirInto.)
func (s *service) CopyInto(stream grpc.ClientStreamingServer[fleetgrpc.CopyIntoChunk, fleetgrpc.CopyIntoReply]) (err error) {
	start := time.Now()
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "copy into: no open header: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "copy into: first chunk must be the open header")
	}
	inst, err := resolveServerInstance(open.GetFleet(), open.GetInstance())
	if err != nil {
		return err
	}
	if inst.Status != fleet.StatusRunning {
		return status.Errorf(codes.FailedPrecondition, "instance %s/%s is not running", open.GetFleet(), open.GetInstance())
	}
	if open.GetIsDir() {
		// Directory copy-in returns here, before the single-file defer below, so
		// log it on its own. The destination dir is the client-requested target.
		err = s.copyDirInto(inst, open, stream)
		logCopyOutcome("in", open.GetFleet(), open.GetInstance(), open.GetDest(), start, err)
		return err
	}
	// Single-file copy-in: register the outcome log now so validation, path
	// resolution (incl. a rejected path-traversal name), and transfer failures
	// are all traced — symmetric with the directory branch above. destPath starts
	// as the client-requested target (the best identifier we have if we fail
	// before resolving) and is replaced with the resolved write path once known,
	// so a successful record names the file actually written — the rename form
	// (dest is a full file path) resolves to a single path, not dest/name.
	destPath := path.Join(open.GetDest(), open.GetName())
	defer func() { logCopyOutcome("in", open.GetFleet(), open.GetInstance(), destPath, start, err) }()
	if err := validateCopyName(open.GetName()); err != nil {
		return err
	}
	if open.GetSize() < 0 {
		return status.Errorf(codes.InvalidArgument, "copy into: negative size %d", open.GetSize())
	}

	finalDir, finalName, err := s.resolveCopyIntoDest(inst, open.GetDest(), open.GetName())
	if err != nil {
		return err
	}
	destPath = path.Join(finalDir, finalName)

	mode := os.FileMode(open.GetMode()).Perm()
	if mode == 0 {
		mode = 0o644
	}

	// Buffer the upload to a host temp, validating the declared size exactly, so a
	// truncated or oversized stream fails the RPC BEFORE anything is written into
	// the instance (preserving any existing destination, the same atomicity the
	// tar path had). Then hand the validated file to the backend's CopyFile, which
	// transfers it with a transport that does not depend on stdin EOF — so it
	// works on the coder backend, whose `coder ssh` never delivers the stdin EOF a
	// `tar -xf -` waits for — and publishes it atomically (same-dir temp + rename).
	hostTmp, written, err := drainCopyIntoToHostFile(stream, open.GetSize())
	if err != nil {
		return err
	}
	defer func() { _ = hostTmp.Close(); _ = os.Remove(hostTmp.Name()) }()

	finalPath := finalName
	if finalDir != "." {
		finalPath = path.Join(finalDir, finalName)
	}
	if err := copyFileInto(inst, hostTmp, finalPath, int(mode)); err != nil {
		return status.Errorf(codes.Internal, "write %q to %s/%s: %v", finalName, open.GetFleet(), open.GetInstance(), err)
	}

	reportedPath := finalPath
	if !path.IsAbs(reportedPath) {
		// A relative dest resolved against the workspace folder (the exec cwd);
		// report the absolute path the file actually landed at.
		reportedPath = path.Join(inst.WorkspaceDir, finalDir, finalName)
	}
	return stream.SendAndClose(&fleetgrpc.CopyIntoReply{Path: reportedPath, Written: written})
}

// copyFileInto is the seam from the CopyInto handler to the backend's stdin-EOF-
// safe file transfer. A package var so tests can stub the whole backend layer
// (mirroring copyIntoExecCommand).
var copyFileInto = func(inst *fleet.Instance, src io.Reader, remotePath string, mode int) error {
	return backendutil.NewForInstance(inst, false).CopyFile(inst.WorkspaceDir, src, remotePath, mode)
}

// drainCopyIntoToHostFile streams the client's data chunks into a host temp file,
// enforcing the declared size exactly: a stream that ends short or overruns is an
// InvalidArgument (so a truncated upload never lands as a silent partial file),
// and any other failure is Internal. On success it returns the temp file
// rewound to the start (ready to be read by CopyFile) and the byte count; the
// caller owns closing and removing it.
//
// Unlike the old tar path (which streamed straight through to the instance), the
// whole upload is buffered to disk first — that is what makes the exact-size
// check possible before anything is written into the instance, and it lets the
// coder backend scp the file without a second copy. The temp lives in os.TempDir
// (honoring $TMPDIR), so a very large `fleet copy` needs host scratch space
// there; point $TMPDIR at a roomy filesystem if the default /tmp is small or
// RAM-backed.
func drainCopyIntoToHostFile(stream copyIntoChunkReceiver, size int64) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "fleetcopyin-*")
	if err != nil {
		return nil, 0, status.Errorf(codes.Internal, "stage upload: %v", err)
	}
	fail := func(c codes.Code, format string, a ...any) (*os.File, int64, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, status.Errorf(c, format, a...)
	}

	var written int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(codes.Internal, "recv: %v", err)
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		written += int64(len(data))
		if written > size {
			return fail(codes.InvalidArgument, "copy into: stream exceeds declared size %d", size)
		}
		if _, err := f.Write(data); err != nil {
			return fail(codes.Internal, "stage upload: %v", err)
		}
	}
	if written != size {
		return fail(codes.InvalidArgument, "copy into: stream ended early — got %d of %d bytes", written, size)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(codes.Internal, "stage upload: %v", err)
	}
	return f, written, nil
}

// copyDirInto extracts the client's tar of directory contents into the instance.
// It resolves a target directory (an existing dest is copied INTO; otherwise dest
// IS the directory), creates it, and pipes the data to `tar -xf - -C <dir>` — an
// in-place merge (cp -r semantics), NOT atomic: a failed extract can leave a
// partial tree. A broken client stream or a non-zero tar fails the RPC loudly.
// Refused on the codespaces backend, whose exec PTY can silently corrupt the tar.
func (s *service) copyDirInto(inst *fleet.Instance, open *fleetgrpc.CopyIntoOpen, stream grpc.ClientStreamingServer[fleetgrpc.CopyIntoChunk, fleetgrpc.CopyIntoReply]) error {
	if inst.Backend == fleet.BackendCodespaces {
		return status.Errorf(codes.FailedPrecondition, "copying a directory is not supported on the codespaces backend")
	}
	// The dir path still pipes a tar to a stdin-reading `tar -xf -`, which hangs
	// forever on coder (its `coder ssh` never delivers the stdin EOF — issue
	// #223). Refuse it cleanly rather than wedging the RPC; the single-file path
	// goes through CopyFile and is unaffected. A CopyFile-based recursive extract
	// is a tracked follow-up.
	if inst.Backend == fleet.BackendCoder {
		return status.Errorf(codes.FailedPrecondition, "copying a directory is not yet supported on the coder backend")
	}
	if err := validateCopyName(open.GetName()); err != nil {
		return err
	}
	targetRoot, err := s.resolveCopyIntoDirDest(inst, open.GetDest(), open.GetName())
	if err != nil {
		return err
	}
	if err := s.mkdirContainerDir(inst, targetRoot); err != nil {
		return err
	}

	cmd := copyIntoExecCommand(inst, []string{"tar", "-xf", "-", "-C", targetRoot})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start copy: %v", err)
	}

	ctx := stream.Context()
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = stdin.Close()
		case <-finished:
		}
	}()

	written, pumpErr := pumpCopyIntoData(stdin, stream)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	close(finished)

	if pumpErr != nil || waitErr != nil {
		_ = cmd.Process.Kill()
		tarMsg := strings.TrimSpace(stderr.String())
		if tarMsg != "" {
			return status.Errorf(codes.Internal, "extract into %s/%s: %s", open.GetFleet(), open.GetInstance(), tarMsg)
		}
		if pumpErr != nil {
			return status.Errorf(codes.Internal, "extract into %s/%s: %v", open.GetFleet(), open.GetInstance(), pumpErr)
		}
		return status.Errorf(codes.Internal, "extract into %s/%s: %v", open.GetFleet(), open.GetInstance(), waitErr)
	}

	finalPath := targetRoot
	if !path.IsAbs(finalPath) {
		finalPath = path.Join(inst.WorkspaceDir, targetRoot)
	}
	return stream.SendAndClose(&fleetgrpc.CopyIntoReply{Path: finalPath, Written: written})
}

// resolveCopyIntoDirDest resolves the target directory inside the container for a
// recursive copy: an empty dest puts the tree at <workspace>/name; an existing
// directory dest is copied INTO (dest/name); otherwise dest IS the directory to
// create. The target's parent must already exist (matching cp -r), else NotFound.
func (s *service) resolveCopyIntoDirDest(inst *fleet.Instance, dest, name string) (string, error) {
	var targetRoot string
	switch {
	case dest == "":
		targetRoot = name
	case s.probeContainerDir(inst, dest):
		targetRoot = path.Join(dest, name)
	default:
		targetRoot = dest
	}
	parent := path.Dir(targetRoot)
	if parent != "." && parent != "/" && !s.probeContainerDir(inst, parent) {
		return "", status.Errorf(codes.NotFound, "destination directory %q does not exist", parent)
	}
	return targetRoot, nil
}

// mkdirContainerDir creates dir (and only its missing leaf — the parent is
// checked separately) inside the instance; `-p` makes it idempotent for the
// merge-into-existing case.
func (s *service) mkdirContainerDir(inst *fleet.Instance, dir string) error {
	cmd := copyIntoExecCommand(inst, []string{"mkdir", "-p", "--", dir})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return status.Errorf(codes.Internal, "create %q: %s", dir, msg)
		}
		return status.Errorf(codes.Internal, "create %q: %v", dir, err)
	}
	return nil
}

// pumpCopyIntoData drains the client's data chunks straight into w (the tar
// extract's stdin), returning the bytes written and the first stream/write error.
func pumpCopyIntoData(w io.Writer, stream copyIntoChunkReceiver) (int64, error) {
	var n int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		m, werr := w.Write(data)
		n += int64(m)
		if werr != nil {
			return n, werr
		}
	}
}

// resolveCopyIntoDest resolves the destination dir + final basename inside the
// container, scp-style: an empty dest (or a dest that is an existing directory)
// keeps the source name; otherwise dest is the full target path. A trailing
// slash forces directory intent, so a non-existent one is a clean error rather
// than silently writing a file named like the missing directory. The chosen
// directory is probed for existence so a missing one is NotFound, not a folded
// tar failure.
func (s *service) resolveCopyIntoDest(inst *fleet.Instance, dest, name string) (finalDir, finalName string, err error) {
	if dest == "" {
		return ".", name, nil
	}
	if s.probeContainerDir(inst, dest) {
		return dest, name, nil
	}
	if strings.HasSuffix(dest, "/") {
		return "", "", status.Errorf(codes.NotFound, "destination directory %q does not exist", strings.TrimRight(dest, "/"))
	}
	finalDir = path.Dir(dest)
	finalName = path.Base(dest)
	if err := validateCopyName(finalName); err != nil {
		return "", "", err
	}
	if finalDir != "." && !s.probeContainerDir(inst, finalDir) {
		return "", "", status.Errorf(codes.NotFound, "destination directory %q does not exist", finalDir)
	}
	return finalDir, finalName, nil
}

// probeContainerDir reports whether dir is an existing directory inside the
// instance, via a `test -d` exec (argv straight to exec, exit code only).
func (s *service) probeContainerDir(inst *fleet.Instance, dir string) bool {
	return copyIntoExecCommand(inst, []string{"test", "-d", dir}).Run() == nil
}

// copyIntoChunkReceiver is the read half of the CopyInto stream — abstracted so
// drainCopyIntoToHostFile (single file) and pumpCopyIntoData (directory) are
// unit-testable without a real gRPC stream.
type copyIntoChunkReceiver interface {
	Recv() (*fleetgrpc.CopyIntoChunk, error)
}

// validateCopyName rejects a basename that is empty, a path-traversal token,
// contains a separator, or carries a control character — a single-file copy must
// never create or escape into subdirectories from the name alone, and a newline
// or NUL would corrupt the coder backend's scp transfer (its legacy-protocol
// fallback frames records by newline, so an embedded one breaks the stream).
func validateCopyName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return status.Errorf(codes.InvalidArgument, "invalid file name %q", name)
	}
	if strings.ContainsAny(name, "\n\r\x00") {
		return status.Errorf(codes.InvalidArgument, "file name %q contains a control character", name)
	}
	return nil
}
