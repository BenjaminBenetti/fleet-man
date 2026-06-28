package server

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
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
// Both move bytes through the backend with a single container exec and a `tar`
// pipe, so the file's metadata (mode, size) and bytes ride one consistent
// stream with no shell quoting (argv goes straight to exec) and no stat/cat
// race. OUT runs `tar -chf -` to stdout (-h dereferences a symlinked source so
// the client receives the real content); IN runs `tar -xf -` from stdin into
// the destination directory, preserving the file mode.
//
// Backend caveat (shared by both directions, pre-existing): the codespaces
// backend allocates a PTY for exec, whose line discipline mangles binary tar —
// on stdin (IN) a 0x04 byte reads as EOF, truncating the upload. The strict size
// check below turns such a truncation into a clean InvalidArgument rather than a
// silent partial write. devcontainer (the default) and coder exec without a PTY
// and over a clean binary channel, so both directions work there.

// copyChunkSize is the data-frame payload size. Comfortably under gRPC's 4MB
// default message cap while keeping per-frame overhead negligible.
const copyChunkSize = 64 * 1024

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
	defer func() {
		if err != nil {
			flog.Error("file copy out failed", "fleet", req.GetFleet(), "instance", req.GetInstance(), "path", filePath, "err", err)
		} else {
			flog.Info("file copied out", "fleet", req.GetFleet(), "instance", req.GetInstance(), "path", filePath, "ms", flog.MillisSince(start))
		}
	}()
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
// carry data. The server resolves dest inside the container, extracts a one-file
// tar (built here from the streamed bytes) with `tar -xf - -C <dir>` under a
// temporary name, then renames it into place — so a truncated or oversized
// stream (the declared size is a hard cap) fails the RPC without ever clobbering
// an existing destination, the same atomicity the download direction has. Only
// single regular files; the mode is preserved.
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
		if err != nil {
			flog.Error("file copy in failed", "fleet", open.GetFleet(), "instance", open.GetInstance(), "path", open.GetDest(), "err", err)
		} else {
			flog.Info("file copied in", "fleet", open.GetFleet(), "instance", open.GetInstance(), "path", open.GetDest(), "ms", flog.MillisSince(start))
		}
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
	defer func() {
		if err != nil {
			flog.Error("file copy in failed", "fleet", open.GetFleet(), "instance", open.GetInstance(), "path", destPath, "err", err)
		} else {
			flog.Info("file copied in", "fleet", open.GetFleet(), "instance", open.GetInstance(), "path", destPath, "ms", flog.MillisSince(start))
		}
	}()
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

	// Extract under a hidden temporary name and rename into place only on a clean
	// finish, so a half-written upload never replaces a previous good file.
	tempName := copyIntoTempName()

	cmd := copyIntoExecCommand(inst, []string{"tar", "-xf", "-", "-C", finalDir})
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
	// On client disconnect, kill tar AND close stdin so a write blocked on a full
	// pipe unblocks (the inverse of CopyFile, where the active side is stdout).
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = stdin.Close()
		case <-finished:
		}
	}()

	writeErr := writeCopyIntoTar(stdin, stream, tempName, mode, open.GetSize())
	// EOF to tar. A no-op if the killer already closed it; harmless after a
	// write error (tar already saw a short archive).
	_ = stdin.Close()
	waitErr := cmd.Wait()
	close(finished)

	tarMsg := strings.TrimSpace(stderr.String())
	if writeErr != nil || waitErr != nil {
		// Never leave the temp file behind on a failed upload.
		s.removeContainerFile(inst, finalDir, tempName)
		if writeErr != nil {
			// A size mismatch is the client's fault and already a clean status.
			if status.Code(writeErr) == codes.InvalidArgument {
				return writeErr
			}
			// Otherwise the write likely hit EPIPE because tar died first — tar's
			// own stderr (e.g. a missing directory) is the real cause.
			if tarMsg != "" {
				return status.Errorf(codes.Internal, "write %q to %s/%s: %s", finalName, open.GetFleet(), open.GetInstance(), tarMsg)
			}
			return writeErr
		}
		if tarMsg != "" {
			return status.Errorf(codes.Internal, "write %q to %s/%s: %s", finalName, open.GetFleet(), open.GetInstance(), tarMsg)
		}
		return status.Errorf(codes.Internal, "write %q to %s/%s: %v", finalName, open.GetFleet(), open.GetInstance(), waitErr)
	}

	// Publish the fully-written file with an atomic rename.
	if err := s.moveContainerFile(inst, finalDir, tempName, finalName); err != nil {
		s.removeContainerFile(inst, finalDir, tempName)
		return status.Errorf(codes.Internal, "finalize %q in %s/%s: %v", finalName, open.GetFleet(), open.GetInstance(), err)
	}

	finalPath := path.Join(finalDir, finalName)
	if !path.IsAbs(finalPath) {
		// A relative dest resolved against the workspace folder (the exec cwd);
		// report the absolute path the file actually landed at.
		finalPath = path.Join(inst.WorkspaceDir, finalDir, finalName)
	}
	return stream.SendAndClose(&fleetgrpc.CopyIntoReply{Path: finalPath, Written: open.GetSize()})
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

// copyIntoSeq makes the temp name below unique within this process regardless of
// the RNG, so two concurrent uploads into one directory never collide even if
// crypto/rand degrades (a failed Read would otherwise leave the bytes zeroed).
var copyIntoSeq atomic.Uint64

// copyIntoTempName returns a hidden, unique basename to extract an upload into
// before the atomic rename onto its real destination.
func copyIntoTempName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf(".fleetcopy-%d-%x", copyIntoSeq.Add(1), b[:])
}

// moveContainerFile renames from→to within dir inside the instance — the atomic
// publish step that replaces any existing destination only once the upload is
// fully written.
func (s *service) moveContainerFile(inst *fleet.Instance, dir, from, to string) error {
	// `--` so a destination basename beginning with '-' is treated as a path,
	// not an mv flag.
	cmd := copyIntoExecCommand(inst, []string{"mv", "-f", "--", path.Join(dir, from), path.Join(dir, to)})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// removeContainerFile best-effort deletes a leftover temp file inside the
// instance after a failed upload.
func (s *service) removeContainerFile(inst *fleet.Instance, dir, name string) {
	_ = copyIntoExecCommand(inst, []string{"rm", "-f", "--", path.Join(dir, name)}).Run()
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
// writeCopyIntoTar is unit-testable without a real gRPC stream.
type copyIntoChunkReceiver interface {
	Recv() (*fleetgrpc.CopyIntoChunk, error)
}

// writeCopyIntoTar drains the client's data chunks into w as a single-file tar
// archive named name with the given mode. It enforces size exactly: a stream
// that ends short or overruns is an InvalidArgument (so a truncated upload never
// lands as a silent partial file), and any other write failure surfaces verbatim.
func writeCopyIntoTar(w io.Writer, stream copyIntoChunkReceiver, name string, mode os.FileMode, size int64) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     int64(mode.Perm()),
		Size:     size,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return status.Errorf(codes.Internal, "tar header: %v", err)
	}

	var written int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "recv: %v", err)
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		written += int64(len(data))
		if written > size {
			return status.Errorf(codes.InvalidArgument, "copy into: stream exceeds declared size %d", size)
		}
		if _, err := tw.Write(data); err != nil {
			return status.Errorf(codes.Internal, "tar write: %v", err)
		}
	}
	if written != size {
		return status.Errorf(codes.InvalidArgument, "copy into: stream ended early — got %d of %d bytes", written, size)
	}
	if err := tw.Close(); err != nil {
		return status.Errorf(codes.Internal, "tar close: %v", err)
	}
	return nil
}

// validateCopyName rejects a basename that is empty, a path-traversal token, or
// contains a separator — a single-file copy must never create or escape into
// subdirectories from the name alone.
func validateCopyName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return status.Errorf(codes.InvalidArgument, "invalid file name %q", name)
	}
	return nil
}
