package kubernetes

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// workspace_tar.go streams the daemon host's checkout of an instance's
// workspace as the tar the seed Job extracts into the instance disk's ws/
// (podseed.ExtractWorkspace): every directory, regular file and symlink,
// paths relative to the checkout ("./README.md"), modes and times kept.
// Owners travel as they are on the host; the seed rewrites them to the
// share uid. Sockets, FIFOs and devices, which a checkout never has, are
// left out.

// workspaceTar opens a tar stream of dir. The stream is made while it is
// read; an error walking dir ends it with that error.
func workspaceTar(dir string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("the workspace checkout: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("the workspace checkout %s is not a directory", dir)
		}
		r, w := io.Pipe()
		go func() { _ = w.CloseWithError(writeWorkspaceTar(w, dir)) }()
		return r, nil
	}
}

func writeWorkspaceTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		link := ""
		switch {
		case mode.IsDir(), mode.IsRegular():
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		default:
			// A socket, FIFO or device: not something a checkout carries.
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		name := "./" + filepath.ToSlash(rel)
		if rel == "." {
			name = "./"
		} else if mode.IsDir() {
			name += "/"
		}
		h.Name = name
		h.Uname, h.Gname = "", ""
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			h.Uid, h.Gid = int(st.Uid), int(st.Gid)
		}
		h.Format = tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(tw, f)
		if err == nil && n != h.Size {
			err = fmt.Errorf("%s changed size while it was sent", rel)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("sending the workspace: %w", err)
	}
	return tw.Close()
}
