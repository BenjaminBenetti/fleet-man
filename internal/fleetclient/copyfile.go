package fleetclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// CopyFileTo pulls fleet/instance:srcPath from the server (the CopyFile RPC)
// and writes it to dest on the local machine. dest may be an existing
// directory (or end in a path separator), in which case the file keeps its
// source basename inside it; an existing destination file is overwritten —
// scp semantics, so re-copying an iterated build always lands in the same
// place. The bytes go to a temp file in the destination directory first and
// are renamed into place on success, so a half-finished copy never replaces a
// previous good one. Returns the final local path and the bytes written.
func CopyFileTo(ctx context.Context, svc fleetgrpc.FleetServiceClient, fleetName, instanceName, srcPath, dest string) (string, int64, error) {
	stream, err := svc.CopyFile(ctx, &fleetgrpc.CopyFileRequest{
		Fleet:    fleetName,
		Instance: instanceName,
		Path:     srcPath,
	})
	if err != nil {
		return "", 0, err
	}

	// The first chunk must be the meta frame (name/mode/size).
	first, err := stream.Recv()
	if err != nil {
		return "", 0, err
	}
	meta := first.GetMeta()
	if meta == nil {
		return "", 0, fmt.Errorf("copy %s/%s:%s: server sent no file metadata", fleetName, instanceName, srcPath)
	}

	destPath, err := ResolveCopyDest(dest, meta.GetName())
	if err != nil {
		return "", 0, err
	}

	mode := os.FileMode(meta.GetMode()).Perm()
	if mode == 0 {
		mode = 0o644
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), "."+filepath.Base(destPath)+".fleetcopy-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		// Best-effort cleanup on every early-error path; succeeds-into-rename
		// leaves nothing for these to do.
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if err := tmp.Chmod(mode); err != nil {
		return "", 0, err
	}

	var written int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
		data := chunk.GetData()
		if len(data) == 0 {
			continue
		}
		n, err := tmp.Write(data)
		written += int64(n)
		if err != nil {
			return "", 0, err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), destPath); err != nil {
		return "", 0, err
	}
	return destPath, written, nil
}

// ResolveCopyDest resolves the local destination path for a copied file named
// name: an empty dest or a dest that is (or is spelled like) a directory keeps
// the source basename inside it.
func ResolveCopyDest(dest, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("server sent an empty file name")
	}
	if dest == "" {
		return name, nil
	}
	if strings.HasSuffix(dest, string(os.PathSeparator)) || strings.HasSuffix(dest, "/") {
		return filepath.Join(dest, name), nil
	}
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		return filepath.Join(dest, name), nil
	}
	return dest, nil
}
