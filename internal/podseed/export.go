package podseed

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// export.go is a rebuild's workspace export (`fleet pod-job ws-export`), the
// inverse of the seed's ExtractWorkspace: the workspace folder as a tar on
// stdout, which fleetd extracts over the daemon host's checkout so the
// rebuild builds from what the instance holds (a devcontainer.json edited in
// the pod). It reads a live workspace, so it never fails on what a user's
// program does meanwhile: a file that vanished is left out, one that grew or
// shrank is sent at the size it had when it was opened (padded with zeros
// or cut), and an entry it may not read is left out and named. A mount
// inside the workspace (a volume over node_modules) is sent as an empty
// directory, as the checkout a docker bind mount shows on the host has it.

// ExportSummary is what ExportWorkspace sent; `ws-export` prints it, after
// the tar, as its last stderr line (ExportSummaryPrefix, then JSON).
type ExportSummary struct {
	// Files counts regular files, Links symlinks, Dirs directories; Bytes
	// is the files' content.
	Files int64 `json:"files"`
	Links int64 `json:"links,omitempty"`
	Dirs  int64 `json:"dirs"`
	Bytes int64 `json:"bytes"`
	// Skipped are entries left out: sockets, FIFOs, devices, and what
	// vanished or could not be read. Unreadable names the first of the
	// latter (UnreadableCap); Mounts the mounts sent empty.
	Skipped    int64    `json:"skipped,omitempty"`
	Unreadable []string `json:"unreadable,omitempty"`
	Mounts     []string `json:"mounts,omitempty"`
	// Size and SHA256 are the tar stream's, for the receiver to check.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ExportSummaryPrefix starts the summary line on stderr.
const ExportSummaryPrefix = "fleet-ws-export: "

// UnreadableCap bounds ExportSummary.Unreadable.
const UnreadableCap = 20

// ExportOptions are an export's.
type ExportOptions struct {
	// Mounts are absolute paths of mounts beneath the dir: each is sent as
	// an empty directory (NestedMounts lists them).
	Mounts []string
}

// ExportWorkspace writes the tar of dir to w: every directory, regular file
// and symlink, paths relative to dir ("./README.md"), modes, owners and times
// kept, in the layout ExtractWorkspace reads.
func ExportWorkspace(w io.Writer, dir string, opts ExportOptions) (ExportSummary, error) {
	var sum ExportSummary
	info, err := os.Stat(dir)
	if err != nil {
		return sum, fmt.Errorf("the workspace: %w", err)
	}
	if !info.IsDir() {
		return sum, fmt.Errorf("the workspace %s is not a directory", dir)
	}
	hw := &hashWriter{w: w, h: sha256.New()}
	tw := tar.NewWriter(hw)
	mounts := map[string]bool{}
	for _, m := range opts.Mounts {
		mounts[filepath.Clean(m)] = true
	}
	unreadable := func(rel string, err error) {
		sum.Skipped++
		if len(sum.Unreadable) < UnreadableCap {
			sum.Unreadable = append(sum.Unreadable, fmt.Sprintf("%s: %v", rel, unwrapPath(err)))
		}
	}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				sum.Skipped++
				return skipOf(d)
			}
			if rel == "." && d == nil {
				return err
			}
			unreadable(rel, err)
			return skipOf(d)
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				sum.Skipped++
				return skipOf(d)
			}
			unreadable(rel, err)
			return skipOf(d)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := writeEntry(tw, rel, info, ""); err != nil {
				return err
			}
			sum.Dirs++
			if rel != "." && mounts[filepath.Clean(path)] {
				sum.Mounts = append(sum.Mounts, filepath.ToSlash(rel))
				return filepath.SkipDir
			}
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					sum.Skipped++
				} else {
					unreadable(rel, err)
				}
				return nil
			}
			if err := writeEntry(tw, rel, info, link); err != nil {
				return err
			}
			sum.Links++
		case mode.IsRegular():
			f, err := os.Open(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					sum.Skipped++
				} else {
					unreadable(rel, err)
				}
				return nil
			}
			n, err := writeFile(tw, rel, f)
			_ = f.Close()
			if err != nil {
				return err
			}
			sum.Files++
			sum.Bytes += n
		default:
			// A socket, FIFO or device: nothing a checkout carries.
			sum.Skipped++
		}
		return nil
	})
	if walkErr != nil {
		return sum, fmt.Errorf("exporting the workspace: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return sum, fmt.Errorf("exporting the workspace: %w", err)
	}
	sum.Size, sum.SHA256 = hw.n, hex.EncodeToString(hw.h.Sum(nil))
	return sum, nil
}

// skipOf skips d's subtree when d is a directory, else just d.
func skipOf(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// unwrapPath is err without the path an fs error repeats.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// writeFile writes the open file f as the entry rel, at the size it has now
// (fstat): a file that shrinks while it is read is padded with zeros, one
// that grows is cut, so a live workspace never breaks the stream. It returns
// the content bytes written.
func writeFile(tw *tar.Writer, rel string, f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return writeSized(tw, rel, info, f)
}

// writeSized writes entry rel for info with info's size of content from r.
// A short read (the file shrank, or a read failed mid-file) leaves the rest
// of what the header promised to zeros; a write that fails (the receiver
// went away) fails the zeros' write too, which is returned.
func writeSized(tw *tar.Writer, rel string, info fs.FileInfo, r io.Reader) (int64, error) {
	if err := writeEntry(tw, rel, info, ""); err != nil {
		return 0, err
	}
	n, _ := io.CopyN(tw, r, info.Size())
	if n < info.Size() {
		if _, err := io.CopyN(tw, zeros{}, info.Size()-n); err != nil {
			return n, err
		}
	}
	return info.Size(), nil
}

// writeEntry writes the header of entry rel for info (link is a symlink's
// target).
func writeEntry(tw *tar.Writer, rel string, info fs.FileInfo, link string) error {
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	name := "./" + filepath.ToSlash(rel)
	switch {
	case rel == ".":
		name = "./"
	case info.IsDir():
		name += "/"
	}
	h.Name = name
	h.Uname, h.Gname = "", ""
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		h.Uid, h.Gid = int(st.Uid), int(st.Gid)
	}
	h.Format = tar.FormatPAX
	return tw.WriteHeader(h)
}

// zeros reads zero bytes forever.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// hashWriter counts and hashes what goes to w.
type hashWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (h *hashWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

// FormatExportSummary is the summary line `ws-export` prints last on stderr.
func FormatExportSummary(s ExportSummary) string {
	data, _ := json.Marshal(s)
	return ExportSummaryPrefix + string(data)
}

// ParseExportSummary reads the summary off an export's stderr: its last
// summary line. ok is false when there is none (the export did not end).
func ParseExportSummary(stderr string) (ExportSummary, bool) {
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, found := strings.CutPrefix(strings.TrimSpace(lines[i]), ExportSummaryPrefix)
		if !found {
			continue
		}
		var s ExportSummary
		if json.Unmarshal([]byte(rest), &s) != nil {
			return ExportSummary{}, false
		}
		return s, true
	}
	return ExportSummary{}, false
}

// DiskSpace is a filesystem's size and the room left on it for an
// unprivileged writer, in bytes.
type DiskSpace struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
}
