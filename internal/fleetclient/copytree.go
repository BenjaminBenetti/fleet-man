package fleetclient

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// copytree.go is the Go-side tar layer for recursive directory copies. The server
// stays a plain `tar` in/out; ALL of the safety work happens here in the client
// (which may be the host CLI or the host TUI), so it is consistent across every
// transport: symlinks and special files are SKIPPED (the simplest safe choice —
// no symlink-escape, no loops), the `./` root entry is dropped (so a merge never
// rewrites the destination directory's own mode), and entry names are sanitized
// so a malicious/compromised server can never escape the target on extract.
//
// Directory copies are an in-place MERGE (cp -r semantics) and are NOT atomic.

// copyChunkWriter adapts the gRPC stream's Send into an io.Writer for a
// tar.Writer, splitting writes to copyChunkSize so no frame exceeds the gRPC cap.
// The bytes are safe to reuse after Send returns (gRPC marshals synchronously).
type copyChunkWriter struct {
	send func([]byte) error
}

func (w copyChunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), copyChunkSize)
		if err := w.send(p[:n]); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// copyChunkReader adapts the gRPC stream's data frames into an io.Reader for a
// tar.Reader. recv returns the next frame's bytes, or a non-nil error (io.EOF at
// the end) that is surfaced only once the buffered bytes are drained.
type copyChunkReader struct {
	recv func() ([]byte, error)
	buf  []byte
	err  error
}

func (r *copyChunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		data, err := r.recv()
		r.buf = data
		if err != nil {
			r.err = err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// copyableTarEntry reports whether a tar entry should be copied: regular files
// and directories only (symlinks, hardlinks, devices and fifos are skipped), and
// never the `./` root entry.
func copyableTarEntry(hdr *tar.Header) bool {
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
		return false
	}
	return tarSlashName(hdr.Name) != ""
}

// tarSlashName cleans a tar entry name to a slash-separated path relative to the
// archive root, with traversal confined: a leading "/" or any ".." is collapsed
// so the result can never escape. Backslashes are folded to "/" so a malicious
// name can't traverse on a Windows client. The root entry collapses to "".
func tarSlashName(name string) string {
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	return strings.TrimPrefix(clean, "/")
}

// extractTarTree extracts the tar on r into targetRoot, creating it first. It
// skips symlinks/special files and the root entry, and writes every entry through
// an os.Root rooted at targetRoot — so no name (../, absolute) and no on-disk
// symlink at ANY path component can escape it. Returns the total regular-file
// bytes written. A truncated archive surfaces as an error (Go's tar.Reader errors
// on a short body, unlike GNU tar).
func extractTarTree(r io.Reader, targetRoot string) (int64, error) {
	root, err := openDestRoot(targetRoot)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	tr := tar.NewReader(r)
	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, err
		}
		if !copyableTarEntry(hdr) {
			continue
		}
		name := filepath.FromSlash(tarSlashName(hdr.Name))
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if mode == 0 {
				mode = 0o755
			}
			if err := root.MkdirAll(name, mode|0o700); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if parent := filepath.Dir(name); parent != "." {
				if err := root.MkdirAll(parent, 0o755); err != nil {
					return written, err
				}
			}
			if mode == 0 {
				mode = 0o644
			}
			n, err := writeRootFile(root, name, tr, mode)
			written += n
			if err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// openDestRoot creates targetRoot (rejecting it if it is itself a symlink) and
// returns an os.Root confining all subsequent writes to it — the boundary that
// stops a write from following an intermediate symlink out of the destination.
func openDestRoot(targetRoot string) (*os.Root, error) {
	if fi, err := os.Lstat(targetRoot); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to copy into %s: it is a symlink", targetRoot)
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(targetRoot)
}

// writeTarTreeFromWalk walks srcDir and writes a tar of its CONTENTS to tw
// (entries relative to srcDir, no root entry), copying regular files and
// directories and SKIPPING symlinks/special files. Returns the regular-file bytes.
func writeTarTreeFromWalk(tw *tar.Writer, srcDir string) (int64, error) {
	var written int64
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == srcDir {
			return nil // the root — its contents go in, not the dir itself
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil // skip symlinks and special files
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n, err := copyFileInto(tw, p)
			written += n
			if err != nil {
				return err
			}
		}
		return nil
	})
	return written, err
}

// filterTarTree copies the tar on tr to tw, keeping only regular files and
// directories (no symlinks/specials, no root entry) and re-emitting names as
// clean, root-relative paths. Used by the instance→instance relay to turn the
// source's raw `tar -C dir -cf - .` stream into the clean tar the destination
// server extracts. Returns the regular-file bytes.
func filterTarTree(tw *tar.Writer, tr *tar.Reader) (int64, error) {
	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if !copyableTarEntry(hdr) {
			continue
		}
		name := tarSlashName(hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			name += "/"
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     hdr.Mode,
			Size:     hdr.Size,
			ModTime:  hdr.ModTime,
			Typeflag: hdr.Typeflag,
		}); err != nil {
			return written, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n, err := io.Copy(tw, tr)
			written += n
			if err != nil {
				return written, err
			}
		}
	}
}

// copyTreeLocal recursively copies srcDir to targetRoot on the local disk (the
// local→local degenerate case), with the SAME skip-symlinks/specials contract as
// the transport directions and the SAME os.Root confinement on the destination.
// In-place merge, non-atomic. Returns file bytes.
func copyTreeLocal(srcDir, targetRoot string) (int64, error) {
	root, err := openDestRoot(targetRoot)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	var written int64
	err = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == srcDir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return root.MkdirAll(rel, info.Mode().Perm()|0o700)
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		n, werr := writeRootFile(root, rel, src, info.Mode().Perm())
		_ = src.Close()
		written += n
		return werr
	})
	return written, err
}

// writeRootFile writes r's bytes to name (relative to root) with mode, confined
// to the os.Root so it can never follow a symlink out of the destination.
func writeRootFile(root *os.Root, name string, r io.Reader, mode os.FileMode) (int64, error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// copyFileInto copies the file at p into w.
func copyFileInto(w io.Writer, p string) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(w, f)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}
