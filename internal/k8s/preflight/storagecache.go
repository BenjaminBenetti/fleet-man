//go:build !nok8s

package preflight

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	storagev1 "k8s.io/api/storage/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// storagecache.go keeps what a connection's storage tests found, on the
// daemon host (state.KubeStorageTestsPath): each StorageClass's multi-node
// test (storagetest.go), keyed on what decides how its volumes behave (its
// provisioner, parameters and mount options), so a class made again with the
// same settings keeps its result and one whose settings changed is tested
// again; and the deep test's two-pod probe of one ReadWriteOnce volume
// (share.go), which a full run without a deep test keeps.

// storageCacheVersion is the cache format this fleet writes and reads.
const storageCacheVersion = 1

// storageTestVersion counts changes to what the multi-node test checks: a
// result from an older test is tested again.
const storageTestVersion = 1

// inconclusiveRetry is how long a test that could not tell (the pods never
// ran, the API failed) is not tried again by the periodic full runs; the
// deep test and an explicit test try at once.
const inconclusiveRetry = time.Hour

// StorageTest is one StorageClass's multi-node test result.
type StorageTest struct {
	// Class is the StorageClass tested, Provisioner its provisioner ("" when
	// StorageClasses are not readable and the class was tested by name).
	Class       string `json:"class"`
	Provisioner string `json:"provisioner,omitempty"`
	// Key is what the result is cached by (StorageTestKey).
	Key string    `json:"key"`
	At  time.Time `json:"at"`
	// MultiNode: two pods on two nodes mounted one ReadWriteMany volume of
	// the class. Nodes are where the test's pods ran.
	MultiNode bool     `json:"multiNode,omitempty"`
	Nodes     []string `json:"nodes,omitempty"`
	// Mounted: both pods mounted the volume and ran the file system checks
	// (on two nodes, or on one when the cluster has one).
	Mounted bool `json:"mounted,omitempty"`
	// Failed is the file system check that failed ("" when none did):
	// such a class is refused for fleet shares.
	Failed string `json:"failed,omitempty"`
	// Detail says what the test found; Fix the remedy, when one is known.
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Inconclusive: the test could not tell (RetryAt is when the periodic
	// runs try it again).
	Inconclusive bool      `json:"inconclusive,omitempty"`
	RetryAt      time.Time `json:"retryAt,omitempty"`
}

// Passed reports a class fleet shares can use from any node.
func (t StorageTest) Passed() bool {
	return t.MultiNode && t.Mounted && t.Failed == "" && !t.Inconclusive
}

// Refused reports a class whose volumes fail the file system checks fleet
// shares need.
func (t StorageTest) Refused() bool {
	return t.Mounted && t.Failed != "" && !t.Inconclusive
}

// StorageTestKey is the cache key of a multi-node test of sc on a cluster of
// one node (multiNode false) or more: the test's version, the provisioner,
// its parameters and mount options. Without the class (StorageClasses not
// readable) the key is the name.
func StorageTestKey(sc *storagev1.StorageClass, name string, multiNode bool) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\x00nodes=%t\x00", storageTestVersion, multiNode)
	if sc == nil {
		fmt.Fprintf(h, "name=%s\x00", name)
	} else {
		fmt.Fprintf(h, "provisioner=%s\x00", sc.Provisioner)
		keys := make([]string, 0, len(sc.Parameters))
		for k := range sc.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "p:%s=%s\x00", k, sc.Parameters[k])
		}
		opts := append([]string(nil), sc.MountOptions...)
		sort.Strings(opts)
		fmt.Fprintf(h, "mount=%s\x00", strings.Join(opts, ","))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// storageCacheFile is the cache's file.
type storageCacheFile struct {
	V           int                    `json:"v"`
	Tests       map[string]StorageTest `json:"tests,omitempty"`
	MultiPodRWO *bool                  `json:"multiPodRWO,omitempty"`
}

// StorageCache is one connection's storage test cache, kept in a file.
type StorageCache struct {
	path string

	mu   sync.Mutex
	data storageCacheFile
}

// OpenStorageCache reads the cache at path. A missing file is an empty
// cache; one that cannot be read or is not this fleet's is started afresh
// (it only ever saves running the tests again).
func OpenStorageCache(path string) *StorageCache {
	c := &StorageCache{path: path, data: storageCacheFile{V: storageCacheVersion}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f storageCacheFile
	if json.Unmarshal(raw, &f) != nil || f.V != storageCacheVersion {
		return c
	}
	c.data = f
	return c
}

// Lookup is the cached result under key, unless it is an inconclusive one
// due again at now.
func (c *StorageCache) Lookup(key string, now time.Time) (StorageTest, bool) {
	if c == nil {
		return StorageTest{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.data.Tests[key]
	if !ok || (t.Inconclusive && !now.Before(t.RetryAt)) {
		return StorageTest{}, false
	}
	return t, true
}

// Record keeps t under its key and saves the cache.
func (c *StorageCache) Record(t StorageTest) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.Tests == nil {
		c.data.Tests = map[string]StorageTest{}
	}
	c.data.Tests[t.Key] = t
	return c.saveLocked()
}

// MultiPodRWO is what the last deep test found of two pods on one node
// mounting one ReadWriteOnce volume (nil when none told).
func (c *StorageCache) MultiPodRWO() *bool {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.MultiPodRWO == nil {
		return nil
	}
	v := *c.data.MultiPodRWO
	return &v
}

// SetMultiPodRWO records the two-pod probe's word and saves the cache.
func (c *StorageCache) SetMultiPodRWO(v bool) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.MultiPodRWO != nil && *c.data.MultiPodRWO == v {
		return nil
	}
	c.data.MultiPodRWO = &v
	return c.saveLocked()
}

func (c *StorageCache) saveLocked() error {
	if c.path == "" {
		return nil
	}
	c.data.V = storageCacheVersion
	raw, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("storage test cache: %w", err)
	}
	if err := atomicfile.Write(c.path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("storage test cache: %w", err)
	}
	return nil
}
