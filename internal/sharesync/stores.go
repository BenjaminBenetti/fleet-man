package sharesync

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// blobStore holds content the hub itself serves, by hash: merged files no
// side holds yet, and the earlier contents a policy keeps for its merges.
type blobStore struct {
	dir string
}

// path returns a blob's file.
func (b *blobStore) path(hash string) string {
	return filepath.Join(b.dir, hash)
}

// hashOf returns data's content hash.
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// put stores data and returns its hash.
func (b *blobStore) put(data []byte) (string, error) {
	hash := hashOf(data)
	if b.has(hash) {
		return hash, nil
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return "", err
	}
	return hash, atomicfile.Write(b.path(hash), data, 0o600)
}

// has reports whether the store holds hash.
func (b *blobStore) has(hash string) bool {
	if !validHash(hash) {
		return false
	}
	_, err := os.Stat(b.path(hash))
	return err == nil
}

// open opens a blob from offset on.
func (b *blobStore) open(hash string, offset int64) (io.ReadCloser, error) {
	if !validHash(hash) {
		return nil, fs.ErrNotExist
	}
	f, err := os.Open(b.path(hash))
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// remove drops one blob.
func (b *blobStore) remove(hash string) {
	if validHash(hash) {
		os.Remove(b.path(hash))
	}
}

// gc removes the blobs keep does not name that are older than before.
func (b *blobStore) gc(keep map[string]bool, before time.Time) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			os.Remove(filepath.Join(b.dir, e.Name()))
		}
	}
}

// Conflict is a version of a path that lost to another, kept on the daemon
// host.
type Conflict struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Root string    `json:"root"`
	Path string    `json:"path"`
	// Location is set for a path of location scope.
	Location string `json:"location,omitempty"`
	// Side is the side whose version this is.
	Side  string `json:"side"`
	Entry Entry  `json:"entry"`
	// Reason says what it lost to.
	Reason string `json:"reason"`
	// File holds the version's content (a file's bytes, a symlink's
	// target).
	File string `json:"file"`
}

// conflictStore keeps the most recent conflict copies, each as <id>.json
// beside <id>.data.
type conflictStore struct {
	dir string
	max int
	mu  sync.Mutex
}

// save keeps a conflict copy of content.
func (c *conflictStore) save(meta Conflict, content io.Reader) (Conflict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return Conflict{}, err
	}
	var nonce [4]byte
	rand.Read(nonce[:])
	meta.ID = meta.Time.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce[:])
	meta.File = filepath.Join(c.dir, meta.ID+".data")
	tmp, err := os.CreateTemp(c.dir, ".conflict-*.tmp")
	if err != nil {
		return Conflict{}, err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Conflict{}, err
	}
	if err := os.Rename(tmp.Name(), meta.File); err != nil {
		return Conflict{}, err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return Conflict{}, err
	}
	if err := atomicfile.Write(filepath.Join(c.dir, meta.ID+".json"), data, 0o600); err != nil {
		os.Remove(meta.File)
		return Conflict{}, err
	}
	c.prune()
	return meta, nil
}

// remove drops a conflict copy (one kept for a decision that did not hold).
func (c *conflictStore) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	os.Remove(filepath.Join(c.dir, id+".json"))
	os.Remove(filepath.Join(c.dir, id+".data"))
}

// list returns the conflict copies, oldest first.
func (c *conflictStore) list() ([]Conflict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read()
}

func (c *conflictStore) read() ([]Conflict, error) {
	entries, err := os.ReadDir(c.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Conflict
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			continue
		}
		var meta Conflict
		if json.Unmarshal(data, &meta) == nil && meta.ID+".json" == e.Name() {
			out = append(out, meta)
		}
	}
	slices.SortFunc(out, func(a, b Conflict) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// prune keeps the newest max copies.
func (c *conflictStore) prune() {
	all, err := c.read()
	if err != nil || len(all) <= c.max {
		return
	}
	for _, old := range all[:len(all)-c.max] {
		os.Remove(filepath.Join(c.dir, old.ID+".json"))
		os.Remove(filepath.Join(c.dir, old.ID+".data"))
	}
}

// newEpoch returns a fresh epoch id.
func newEpoch() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// FleetDir returns a fleet's sync state folder beneath base (~/.fleet/sync),
// refusing a fleet name that would not stay one folder there.
func FleetDir(base, fleetName string) (string, error) {
	if err := fleet.ValidateFleetName(fleetName); err != nil {
		return "", fmt.Errorf("sharesync: %w", err)
	}
	if strings.ContainsRune(fleetName, 0) {
		return "", fmt.Errorf("sharesync: fleet name %q holds a NUL", fleetName)
	}
	return filepath.Join(base, fleetName), nil
}
