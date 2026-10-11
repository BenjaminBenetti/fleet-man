package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// seedcache_index.go is the daemon's record of its seed caches on one
// connection, ~/.fleet/kube/<cluster>/seedcaches.json: the instances each
// cache made (its references) and since when none of them is alive. A cache
// unreferenced for SeedCacheTTL is deleted (seedcache.go).

// SeedCacheIndexVersion is the seedcaches.json version this fleet writes.
const SeedCacheIndexVersion = 1

// ErrSeedCacheIndexNewer is a seedcaches.json a newer fleet wrote; it is
// left alone.
var ErrSeedCacheIndexNewer = errors.New("the seed cache index was written by a newer fleet")

// SeedCacheIndex is seedcaches.json.
type SeedCacheIndex struct {
	V      int              `json:"v"`
	Caches []SeedCacheEntry `json:"caches"`
}

// SeedCacheEntry is one seed cache.
type SeedCacheEntry struct {
	// Name is its disk's (names.SeedCache(ID)); Image the image whose root
	// it holds.
	Name  string `json:"name"`
	ID    string `json:"id"`
	Image string `json:"image,omitempty"`
	// Created is when it was built, LastUsed when an instance was last made
	// from it.
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
	// Refs are the keys of the instances made from it (and the one it was
	// made from) that were alive at the last look.
	Refs []string `json:"refs,omitempty"`
	// Unreferenced is when a collection first found none of Refs alive;
	// zero while one is.
	Unreferenced time.Time `json:"unreferenced,omitzero"`
}

// SeedCacheIndexPath is where a connection's index is; a var for tests.
var SeedCacheIndexPath = state.KubeSeedCachesPath

// seedCacheIndexMu serializes the read-modify-writes of every index.
var seedCacheIndexMu sync.Mutex

// LoadSeedCacheIndex reads connection cluster's index; a missing file is an
// empty one.
func LoadSeedCacheIndex(cluster string) (*SeedCacheIndex, error) {
	data, err := os.ReadFile(SeedCacheIndexPath(cluster))
	if errors.Is(err, fs.ErrNotExist) {
		return &SeedCacheIndex{V: SeedCacheIndexVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	ix := &SeedCacheIndex{}
	if err := json.Unmarshal(data, ix); err != nil {
		return nil, fmt.Errorf("parse the seed cache index: %w", err)
	}
	if ix.V > SeedCacheIndexVersion {
		return nil, ErrSeedCacheIndexNewer
	}
	ix.V = SeedCacheIndexVersion
	return ix, nil
}

// UpdateSeedCacheIndex changes connection cluster's index with fn and writes
// it (0600, through atomicfile), unless fn fails.
func UpdateSeedCacheIndex(cluster string, fn func(*SeedCacheIndex) error) error {
	seedCacheIndexMu.Lock()
	defer seedCacheIndexMu.Unlock()
	ix, err := LoadSeedCacheIndex(cluster)
	if err != nil {
		return err
	}
	if err := fn(ix); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	path := SeedCacheIndexPath(cluster)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}

// Find is the entry of the cache whose disk is name, or nil.
func (ix *SeedCacheIndex) Find(name string) *SeedCacheEntry {
	for i := range ix.Caches {
		if ix.Caches[i].Name == name {
			return &ix.Caches[i]
		}
	}
	return nil
}

// Use records that instance key was made from (or made) the cache, now.
func (ix *SeedCacheIndex) Use(name, id, image, key string, now time.Time) {
	e := ix.Find(name)
	if e == nil {
		ix.Caches = append(ix.Caches, SeedCacheEntry{Name: name, ID: id, Image: image, Created: now})
		e = &ix.Caches[len(ix.Caches)-1]
	}
	if key != "" && !slices.Contains(e.Refs, key) {
		e.Refs = append(e.Refs, key)
	}
	e.LastUsed, e.Unreferenced = now, time.Time{}
}

// Remove drops the entry of the cache whose disk is name.
func (ix *SeedCacheIndex) Remove(name string) {
	ix.Caches = slices.DeleteFunc(ix.Caches, func(e SeedCacheEntry) bool { return e.Name == name })
}

// expired keeps only e's refs that are alive and reports whether e has been
// unreferenced for ttl: the first look that finds none alive starts the
// clock.
func (e *SeedCacheEntry) expired(live func(key string) bool, now time.Time, ttl time.Duration) bool {
	e.Refs = slices.DeleteFunc(e.Refs, func(key string) bool { return !live(key) })
	switch {
	case len(e.Refs) > 0:
		e.Unreferenced = time.Time{}
		return false
	case e.Unreferenced.IsZero():
		e.Unreferenced = now
		return false
	}
	return now.Sub(e.Unreferenced) >= ttl
}
