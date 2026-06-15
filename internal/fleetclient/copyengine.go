package fleetclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// copyengine.go is the generic, single-orchestrator copy engine behind scp-style
// `fleet copy`. It moves one file between two RESOLVED endpoints (a front-end has
// already turned `[fleet/]instance:path`, a plain path, or `:path` into a concrete
// local-path or fleet/instance/path), dispatching on which sides are local vs
// instances. The same engine runs in two front-ends: the host `fleet` CLI (with
// its own disk as "local") and the host TUI (running a delegated in-instance copy,
// with the human's disk as "local"). Instance endpoints are reached only over the
// CopyFile/CopyInto RPCs, so the engine works unchanged against a remote fleetd.

// copyChunkSize bounds one CopyInto data frame — matches the server's read size
// and is comfortably under gRPC's 4 MB message ceiling.
const copyChunkSize = 64 * 1024

// ResolvedEndpoint is one side of a copy after a front-end has resolved any
// fleet/self reference: either a local path (Local true) or a path inside a
// concrete instance.
type ResolvedEndpoint struct {
	Local    bool
	Fleet    string
	Instance string
	Path     string
}

// CopyLocalPolicy maps typed local paths onto the orchestrating client's
// filesystem. The host CLI resolves relative to the process cwd; the host TUI
// (running a delegated in-instance copy) resolves relative to the human's home
// and downloads folder. Keeping it injectable is what lets one engine serve both.
type CopyLocalPolicy interface {
	// ResolveSrc maps a typed local SOURCE path to the absolute path to read.
	ResolveSrc(path string) string
	// ResolveDest maps a typed local DEST to the absolute path to write, given
	// the source basename — an empty dest, or a dest that is (or is spelled like)
	// a directory, keeps that name.
	ResolveDest(dest, srcName string) (string, error)
}

// CopyResult reports where a copy landed and how many file bytes moved.
type CopyResult struct {
	DestPath string // the final path written (local abs path, or in-instance abs path)
	Written  int64
}

// Copy performs one scp-style copy between two resolved endpoints, dispatching on
// which sides are local vs instances:
//
//   - local → instance: upload via the CopyInto RPC,
//   - instance → local: download via the CopyFile RPC,
//   - instance → instance: relay one CopyFile stream into one CopyInto stream,
//   - local → local: a plain file copy on the orchestrator's disk.
func Copy(ctx context.Context, svc fleetgrpc.FleetServiceClient, src, dst ResolvedEndpoint, policy CopyLocalPolicy) (CopyResult, error) {
	switch {
	case src.Local && !dst.Local:
		return uploadLocalToInstance(ctx, svc, policy.ResolveSrc(src.Path), dst)
	case !src.Local && dst.Local:
		return downloadInstanceToLocal(ctx, svc, src, dst.Path, policy)
	case !src.Local && !dst.Local:
		return relayInstanceToInstance(ctx, svc, src, dst)
	default:
		return copyLocalToLocal(policy.ResolveSrc(src.Path), dst.Path, policy)
	}
}

// uploadLocalToInstance streams srcPath into dst over the CopyInto RPC. The local
// file is stat'd up front so its size rides the open header (the server needs it
// for the tar archive and enforces it as a hard cap).
func uploadLocalToInstance(ctx context.Context, svc fleetgrpc.FleetServiceClient, srcPath string, dst ResolvedEndpoint) (CopyResult, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return CopyResult{}, err
	}
	if err := requireRegularFile(srcPath, fi); err != nil {
		return CopyResult{}, err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return CopyResult{}, err
	}
	defer f.Close()

	stream, err := svc.CopyInto(ctx)
	if err != nil {
		return CopyResult{}, err
	}
	if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Open{Open: &fleetgrpc.CopyIntoOpen{
		Fleet:    dst.Fleet,
		Instance: dst.Instance,
		Dest:     dst.Path,
		Name:     filepath.Base(srcPath),
		Mode:     uint32(fi.Mode().Perm()),
		Size:     fi.Size(),
	}}}); err != nil {
		return CopyResult{}, err
	}

	buf := make([]byte, copyChunkSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: buf[:n]}}); err != nil {
				return CopyResult{}, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return CopyResult{}, readErr
		}
	}
	reply, err := stream.CloseAndRecv()
	if err != nil {
		return CopyResult{}, err
	}
	return CopyResult{DestPath: reply.GetPath(), Written: reply.GetWritten()}, nil
}

// downloadInstanceToLocal pulls src over the CopyFile RPC and writes it to the
// path the policy resolves for the source basename. The bytes go to a temp file
// in the destination directory first and are renamed into place on success, so a
// half-finished copy never replaces a previous good one (scp semantics).
func downloadInstanceToLocal(ctx context.Context, svc fleetgrpc.FleetServiceClient, src ResolvedEndpoint, dstTyped string, policy CopyLocalPolicy) (CopyResult, error) {
	stream, err := svc.CopyFile(ctx, &fleetgrpc.CopyFileRequest{Fleet: src.Fleet, Instance: src.Instance, Path: src.Path})
	if err != nil {
		return CopyResult{}, err
	}
	first, err := stream.Recv()
	if err != nil {
		return CopyResult{}, err
	}
	meta := first.GetMeta()
	if meta == nil {
		return CopyResult{}, fmt.Errorf("copy %s/%s:%s: server sent no file metadata", src.Fleet, src.Instance, src.Path)
	}

	// The default local filename comes from the server; reduce it to a bare
	// basename so a malicious/compromised server cannot steer a directory dest
	// into an arbitrary path on this machine (e.g. name "../../.ssh/authorized_keys").
	name := filepath.Base(meta.GetName())
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return CopyResult{}, fmt.Errorf("copy %s/%s:%s: server sent an invalid file name %q", src.Fleet, src.Instance, src.Path, meta.GetName())
	}
	destPath, err := policy.ResolveDest(dstTyped, name)
	if err != nil {
		return CopyResult{}, err
	}
	mode := os.FileMode(meta.GetMode()).Perm()
	if mode == 0 {
		mode = 0o644
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), "."+filepath.Base(destPath)+".fleetcopy-*")
	if err != nil {
		return CopyResult{}, err
	}
	defer func() {
		// Best-effort cleanup on every early-error path; a clean rename leaves
		// nothing for these to do.
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(mode); err != nil {
		return CopyResult{}, err
	}

	var written int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return CopyResult{}, err
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		n, err := tmp.Write(data)
		written += int64(n)
		if err != nil {
			return CopyResult{}, err
		}
	}
	if err := tmp.Close(); err != nil {
		return CopyResult{}, err
	}
	if err := os.Rename(tmp.Name(), destPath); err != nil {
		return CopyResult{}, err
	}
	return CopyResult{DestPath: destPath, Written: written}, nil
}

// relayInstanceToInstance copies between two instances without touching local
// disk: the source's CopyFile stream is pumped straight into the destination's
// CopyInto stream, with the source meta (name/mode/size) seeding the open header.
func relayInstanceToInstance(ctx context.Context, svc fleetgrpc.FleetServiceClient, src, dst ResolvedEndpoint) (CopyResult, error) {
	out, err := svc.CopyFile(ctx, &fleetgrpc.CopyFileRequest{Fleet: src.Fleet, Instance: src.Instance, Path: src.Path})
	if err != nil {
		return CopyResult{}, err
	}
	first, err := out.Recv()
	if err != nil {
		return CopyResult{}, err
	}
	meta := first.GetMeta()
	if meta == nil {
		return CopyResult{}, fmt.Errorf("copy %s/%s:%s: server sent no file metadata", src.Fleet, src.Instance, src.Path)
	}

	in, err := svc.CopyInto(ctx)
	if err != nil {
		return CopyResult{}, err
	}
	if err := in.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Open{Open: &fleetgrpc.CopyIntoOpen{
		Fleet:    dst.Fleet,
		Instance: dst.Instance,
		Dest:     dst.Path,
		Name:     meta.GetName(),
		Mode:     meta.GetMode(),
		Size:     meta.GetSize(),
	}}}); err != nil {
		return CopyResult{}, err
	}
	for {
		chunk, err := out.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return CopyResult{}, err
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		if err := in.Send(&fleetgrpc.CopyIntoChunk{Msg: &fleetgrpc.CopyIntoChunk_Data{Data: data}}); err != nil {
			return CopyResult{}, err
		}
	}
	reply, err := in.CloseAndRecv()
	if err != nil {
		return CopyResult{}, err
	}
	return CopyResult{DestPath: reply.GetPath(), Written: reply.GetWritten()}, nil
}

// copyLocalToLocal copies one local file to another on the orchestrator's disk —
// the degenerate case where neither endpoint is an instance. The bytes are
// streamed (not slurped) so a large file does not balloon memory.
func copyLocalToLocal(srcPath, dstTyped string, policy CopyLocalPolicy) (CopyResult, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return CopyResult{}, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return CopyResult{}, err
	}
	if err := requireRegularFile(srcPath, fi); err != nil {
		return CopyResult{}, err
	}
	destPath, err := policy.ResolveDest(dstTyped, filepath.Base(srcPath))
	if err != nil {
		return CopyResult{}, err
	}
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return CopyResult{}, err
	}
	written, copyErr := io.Copy(out, in)
	if copyErr == nil {
		// Set the mode explicitly so it is preserved even when overwriting.
		copyErr = out.Chmod(fi.Mode().Perm())
	}
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return CopyResult{}, copyErr
	}
	return CopyResult{DestPath: destPath, Written: written}, nil
}

// requireRegularFile rejects a local source that is a directory or special file —
// only single regular files can be copied, matching the instance side.
func requireRegularFile(path string, fi os.FileInfo) error {
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory — only single files can be copied", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}
