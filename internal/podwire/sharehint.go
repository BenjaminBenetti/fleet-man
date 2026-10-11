package podwire

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// A share-hint stream (TagShareHint) carries a pod's change hints on a
// fleet share mounted from several nodes (multi-node mode). The pod that
// runs the share's spoke sees the share's changes through inotify only when
// they are made on its own node; a write from a pod on another node reaches
// the volume, but no event. So every other pod's agent watches the share
// itself (DesiredState.ShareHint names the folders) and tells fleetd what
// changed there, which fleetd hands the spoke as a hint to scan those paths
// (sharesync's Side.Hint). The agent opens the stream with an empty
// SockOpen; fleetd answers OPEN (SockReply), then the agent writes one
// ShareHintMsg per line. A hint only ever starts a scan, which reads what
// is there, so a pod can make the spoke do no more than its periodic
// rescan would.

// ShareHint is what DesiredState asks of the agent: the share folders whose
// changes it hints (nil: none, the pod's share is not mounted from several
// nodes).
type ShareHint struct {
	Roots []ShareHintRoot `json:"roots"`
}

// ShareHintRoot is one synced folder of the share: roots/Name below the
// pod's share mount. Skip are folders below it another root covers.
type ShareHintRoot struct {
	Name string   `json:"name"`
	Skip []string `json:"skip,omitempty"`
}

// ShareHintMsg is one hint: paths of root (relative to its folder,
// slash-separated) that changed; "." is all of it.
type ShareHintMsg struct {
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
}

// Limits on share hints: a hint names at most MaxHintPaths paths (more is
// sent as "."), a line holds at most maxHintLine bytes, and a state names at
// most MaxHintRoots folders.
const (
	MaxHintPaths = 256
	MaxHintRoots = 64
	maxHintLine  = 512 << 10
	maxHintPath  = 4096
)

// ValidSharePath reports whether p is a clean slash-separated path below a
// folder (or "." for the folder itself).
func ValidSharePath(p string) bool {
	if p == "." {
		return true
	}
	return p != "" && len(p) <= maxHintPath && !path.IsAbs(p) && path.Clean(p) == p && p != ".." &&
		!strings.HasPrefix(p, "../") && !strings.ContainsAny(p, "\x00\n")
}

// Validate checks the folders: clean, distinct, within the limits.
func (h *ShareHint) Validate() error {
	if h == nil {
		return nil
	}
	if len(h.Roots) > MaxHintRoots {
		return fmt.Errorf("%d share hint folders, at most %d", len(h.Roots), MaxHintRoots)
	}
	seen := map[string]bool{}
	for _, r := range h.Roots {
		if r.Name == "." || !ValidSharePath(r.Name) || seen[r.Name] {
			return fmt.Errorf("share hint folder %q is not a clean path, or named twice", r.Name)
		}
		seen[r.Name] = true
		for _, s := range r.Skip {
			if s == "." || !ValidSharePath(s) {
				return fmt.Errorf("share hint folder %s: skip %q is not a clean path below it", r.Name, s)
			}
		}
	}
	return nil
}

// Validate checks a hint is one fleetd may pass on: a clean root, clean
// paths, within the limits.
func (m ShareHintMsg) Validate() error {
	if m.Root == "." || !ValidSharePath(m.Root) {
		return fmt.Errorf("a share hint for folder %q", m.Root)
	}
	if len(m.Paths) == 0 || len(m.Paths) > MaxHintPaths {
		return fmt.Errorf("a share hint of %d paths (1 to %d)", len(m.Paths), MaxHintPaths)
	}
	for _, p := range m.Paths {
		if !ValidSharePath(p) {
			return fmt.Errorf("a share hint for path %q", p)
		}
	}
	return nil
}

// WriteShareHint writes one hint line.
func WriteShareHint(w io.Writer, m ShareHintMsg) error {
	if len(m.Paths) > MaxHintPaths {
		m.Paths = []string{"."}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) >= maxHintLine {
		m.Paths = []string{"."}
		if data, err = json.Marshal(m); err != nil {
			return err
		}
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// ShareHintReader reads hint lines.
type ShareHintReader struct{ r *bufio.Reader }

// NewShareHintReader reads hints from r.
func NewShareHintReader(r io.Reader) *ShareHintReader {
	return &ShareHintReader{r: bufio.NewReaderSize(r, 32<<10)}
}

// ErrHintTooLong is a hint line over the limit.
var ErrHintTooLong = errors.New("a share hint line is too long")

// Next reads the next hint; a line that is not one is an error, the end of
// the stream io.EOF.
func (h *ShareHintReader) Next() (ShareHintMsg, error) {
	var line []byte
	for {
		chunk, err := h.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxHintLine {
			return ShareHintMsg{}, ErrHintTooLong
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return ShareHintMsg{}, io.ErrUnexpectedEOF
		}
		return ShareHintMsg{}, err
	}
	var m ShareHintMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return ShareHintMsg{}, fmt.Errorf("a share hint line: %w", err)
	}
	return m, m.Validate()
}
