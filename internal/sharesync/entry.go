package sharesync

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Kind is what a path holds.
type Kind uint8

const (
	// Gone is nothing: a delete in a change, a tombstone in the hub's state.
	Gone Kind = iota
	// File is a regular file.
	File
	// Dir is a folder.
	Dir
	// Symlink is a symbolic link, synced as its target string and never
	// followed. Other kinds (FIFOs, sockets, devices) are never synced.
	Symlink
)

// String returns the kind's name for logs and errors.
func (k Kind) String() string {
	switch k {
	case Gone:
		return "gone"
	case File:
		return "file"
	case Dir:
		return "dir"
	case Symlink:
		return "symlink"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Entry is the synced state of one path. Two entries with the same identity
// (Same) are the same content, whatever their modification times.
type Entry struct {
	Kind Kind `json:"k,omitempty"`
	// Mode holds permission bits, plus the sticky bit on a folder. Set-ID
	// bits are never synced. A folder's tombstone keeps the folder's mode.
	Mode fs.FileMode `json:"m,omitempty"`
	// Size is a file's length in bytes.
	Size int64 `json:"s,omitempty"`
	// MTime is a file's modification time in Unix nanoseconds; mirror's
	// newest change wins by it.
	MTime int64 `json:"t,omitempty"`
	// Hash is the hex sha256 of a file's content.
	Hash string `json:"h,omitempty"`
	// Target is a symlink's target, as stored.
	Target string `json:"l,omitempty"`
	// Grew lists earlier contents of this file that its content starts
	// with, oldest first: a copy holding one of them takes only the bytes
	// after it (an appended transcript sends its new lines, not the file).
	// Each claim is checked by the copy that relies on it.
	Grew []Prefix `json:"g,omitempty"`
}

// Prefix is an earlier content of a file, by length and hash.
type Prefix struct {
	Size int64  `json:"s"`
	Hash string `json:"h"`
}

// maxGrew bounds Entry.Grew.
const maxGrew = 16

// maxTarget bounds a symlink target accepted from a side (PATH_MAX).
const maxTarget = 4096

// Live reports whether the entry is something rather than a delete.
func (e Entry) Live() bool {
	return e.Kind != Gone
}

// Same reports whether e and o are the same content: the same kind, mode,
// file content and symlink target. Two deletes are the same.
func (e Entry) Same(o Entry) bool {
	if e.Kind != o.Kind {
		return false
	}
	switch e.Kind {
	case Gone:
		return true
	case File:
		return e.Mode == o.Mode && e.Hash == o.Hash && e.Size == o.Size
	case Dir:
		return e.Mode == o.Mode
	case Symlink:
		return e.Target == o.Target
	}
	return false
}

// extends returns the offset after which e's content continues held, a
// content e lists in Grew, or -1.
func (e Entry) extends(held Entry) int64 {
	if e.Kind != File || held.Kind != File || held.Size >= e.Size {
		return -1
	}
	for _, p := range e.Grew {
		if p.Size == held.Size && p.Hash == held.Hash {
			return p.Size
		}
	}
	return -1
}

// grownFrom returns e with prev's content recorded in Grew: e's content
// starts with prev's.
func (e Entry) grownFrom(prev Entry) Entry {
	grew := append(append([]Prefix(nil), prev.Grew...), Prefix{Size: prev.Size, Hash: prev.Hash})
	if len(grew) > maxGrew {
		grew = grew[len(grew)-maxGrew:]
	}
	e.Grew = grew
	return e
}

// tombstone returns the delete of a path that last held e. It keeps a
// folder's mode, for the folder coming back with an entry made in it
// meanwhile.
func (e Entry) tombstone() Entry {
	if e.Kind != Dir {
		return Entry{Kind: Gone}
	}
	return Entry{Kind: Gone, Mode: e.Mode}
}

// fileMode returns the mode bits sharesync keeps for an entry of kind k.
func fileMode(k Kind, mode fs.FileMode) fs.FileMode {
	switch k {
	case File:
		return mode & fs.ModePerm
	case Dir:
		return mode & (fs.ModePerm | fs.ModeSticky)
	}
	return 0
}

// check refuses an entry arriving from a side that sharesync could not apply
// safely or that cannot be what a scan reports.
func (e Entry) check() error {
	switch e.Kind {
	case Gone:
		if e.Mode&^(fs.ModePerm|fs.ModeSticky) != 0 {
			return fmt.Errorf("delete with mode %v", e.Mode)
		}
		return nil
	case File:
		if e.Mode != fileMode(File, e.Mode) {
			return fmt.Errorf("file mode %v", e.Mode)
		}
		if e.Size < 0 {
			return fmt.Errorf("file size %d", e.Size)
		}
		if !validHash(e.Hash) {
			return fmt.Errorf("file hash %q", e.Hash)
		}
		if len(e.Grew) > maxGrew {
			return fmt.Errorf("%d grown prefixes", len(e.Grew))
		}
		for _, p := range e.Grew {
			if p.Size < 0 || p.Size >= e.Size || !validHash(p.Hash) {
				return fmt.Errorf("grown prefix %+v", p)
			}
		}
	case Dir:
		if e.Mode != fileMode(Dir, e.Mode) {
			return fmt.Errorf("dir mode %v", e.Mode)
		}
	case Symlink:
		if e.Target == "" || len(e.Target) > maxTarget || strings.ContainsRune(e.Target, 0) {
			return fmt.Errorf("symlink target of %d bytes", len(e.Target))
		}
	default:
		return fmt.Errorf("unknown kind %d", e.Kind)
	}
	return nil
}

// validHash reports whether h is a hex sha256.
func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && strings.ToLower(h) == h
}

// validPath reports whether name is a path sharesync syncs beneath a root:
// slash-separated, relative, in the io/fs form, and not the root itself.
func validPath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsRune(name, 0)
}

// ancestors returns the folders above name beneath its root, shallowest
// first ("a/b/c" gives "a", "a/b").
func ancestors(name string) []string {
	var out []string
	for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
		out = append(out, dir)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// within reports whether name is folder or beneath it.
func within(name, folder string) bool {
	return folder == "." || name == folder || strings.HasPrefix(name, folder+"/")
}
