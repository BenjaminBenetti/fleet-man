package kubeimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// indexVersion is the images.json format.
const indexVersion = 1

// Index is one connection's image index (~/.fleet/kube/<cluster
// id>/images.json): every image the daemon delivered to the cluster, what
// uses it, and what it found out about the registries it pushes to. It is
// daemon-owned and backed up with the rest of the Kubernetes state.
type Index struct {
	Version int     `json:"version"`
	Images  []Entry `json:"images,omitempty"`
	// Registries are what the daemon knows about each registry it pushes to,
	// by domain.
	Registries map[string]RegistryState `json:"registries,omitempty"`
	// Configs are, by config digest, the digests the connection's images of
	// that content were delivered under (Entry.Digest, Entry.Manifest), kept
	// while an entry of that content remains: a node that pulled the
	// content under one of them reports it for a pod that pins another,
	// even once the entry that delivered it was collected (identity.go).
	Configs map[string][]string `json:"configs,omitempty"`
}

// Entry is one delivered image.
type Entry struct {
	// ImageID is the image's ID on the daemon host (docker image inspect).
	ImageID  string   `json:"image_id"`
	Strategy Strategy `json:"strategy"`
	// Location is where the image went: the repository it was pushed to (or
	// passes through from), or the local cluster it was loaded into
	// ("minikube:<profile>").
	Location string `json:"location"`
	// Ref is the image the pods name.
	Ref string `json:"ref"`
	// Tag is the tag fleet put on the image on the daemon host (registry and
	// node load), which the GC removes again.
	Tag string `json:"tag,omitempty"`
	// Digest is the manifest digest Ref pins; "" for node load.
	Digest   string `json:"digest,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Manifest and Config are what Digest is on Platform, as fleet read it
	// back after delivering it: the platform manifest's digest (Digest
	// itself unless Digest is an index's) and its config digest, the image's
	// content. "" for node load, and when the registry could not say (an
	// entry an older fleet recorded gets them at its next delivery).
	Manifest string `json:"manifest,omitempty"`
	Config   string `json:"config,omitempty"`
	// PullSecret is a Secret the pods need beyond the connection's own
	// (the gateway's).
	PullSecret string `json:"pull_secret,omitempty"`
	// DeliveredAt is the last delivery (or re-delivery).
	DeliveredAt time.Time `json:"delivered_at"`
	// Holders are what uses the image (instance keys, seed caches). An image
	// with holders is never collected.
	Holders []string `json:"holders,omitempty"`
	// ReleasedAt is when the last holder let go; the image is collected once
	// it has been unheld for the cluster's retention.
	ReleasedAt time.Time `json:"released_at,omitzero"`
	// GCError is why the last collection could not delete the image.
	GCError string `json:"gc_error,omitempty"`
}

// key identifies an entry within its index.
func (e *Entry) key() string { return string(e.Strategy) + "|" + e.Location + "|" + e.ImageID }

// lockKey is what delivery and collection of the entry serialize on, across
// every connection of the daemon: connections that push to one repository,
// or load into one local cluster, share what their entries name, so one
// connection's GC must never delete what another's delivery is handing out.
// Node loads of an image into any local cluster share its one host tag.
func (e *Entry) lockKey() string {
	if e.Strategy == StrategyNodeLoad {
		return string(e.Strategy) + "|" + e.ImageID
	}
	return e.key()
}

// hold adds holder.
func (e *Entry) hold(holder string) {
	if !slices.Contains(e.Holders, holder) {
		e.Holders = append(e.Holders, holder)
		slices.Sort(e.Holders)
	}
	e.ReleasedAt = time.Time{}
}

// release drops holder, starting the retention clock when it was the last.
// It reports whether holder held the image.
func (e *Entry) release(holder string, now time.Time) bool {
	i := slices.Index(e.Holders, holder)
	if i < 0 {
		return false
	}
	e.Holders = slices.Delete(e.Holders, i, i+1)
	if len(e.Holders) == 0 {
		e.Holders = nil
		e.ReleasedAt = now
	}
	return true
}

// unheldSince is when the entry last had a holder or was delivered,
// whichever came later: an image delivered again is in use again, even
// before its new holder takes it.
func (e *Entry) unheldSince() time.Time {
	if e.ReleasedAt.After(e.DeliveredAt) {
		return e.ReleasedAt
	}
	return e.DeliveredAt
}

// RegistryState is what the daemon found out about one registry.
type RegistryState struct {
	// DeleteSupported is whether the registry deletes manifests for the
	// daemon's credential; nil until checked.
	DeleteSupported *bool `json:"delete_supported,omitempty"`
	// Reason says why not.
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

func (ix *Index) find(key string) *Entry {
	for i := range ix.Images {
		if ix.Images[i].key() == key {
			return &ix.Images[i]
		}
	}
	return nil
}

// put adds or replaces the entry with e's key, keeping e's holders as they
// are in the index, and records the digests it was delivered under by its
// config.
func (ix *Index) put(e Entry) *Entry {
	ix.noteConfig(&e)
	if cur := ix.find(e.key()); cur != nil {
		e.Holders, e.ReleasedAt = cur.Holders, cur.ReleasedAt
		*cur = e
		ix.pruneConfigs()
		return cur
	}
	ix.Images = append(ix.Images, e)
	return &ix.Images[len(ix.Images)-1]
}

func (ix *Index) remove(key string) {
	ix.Images = slices.DeleteFunc(ix.Images, func(e Entry) bool { return e.key() == key })
	ix.pruneConfigs()
}

// indexFiles serializes access to index files: one mutex per path, held for
// a whole read-modify-write.
type indexFiles struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (f *indexFiles) lock(path string) func() {
	f.mu.Lock()
	if f.locks == nil {
		f.locks = map[string]*sync.Mutex{}
	}
	l := f.locks[path]
	if l == nil {
		l = &sync.Mutex{}
		f.locks[path] = l
	}
	f.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// update runs fn on the index at path and saves it when fn returns nil and
// changed the index.
func (f *indexFiles) update(path string, fn func(*Index) error) error {
	unlock := f.lock(path)
	defer unlock()
	ix, err := loadIndex(path)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(ix)
	if err := fn(ix); err != nil {
		return err
	}
	if after, _ := json.Marshal(ix); string(after) == string(before) {
		return nil
	}
	return saveIndex(path, ix)
}

// read returns a copy of the index at path.
func (f *indexFiles) read(path string) (*Index, error) {
	unlock := f.lock(path)
	defer unlock()
	return loadIndex(path)
}

// loadIndex reads an index; a missing file is an empty index. A file that
// does not parse is moved aside (images.json.corrupt-<unix time>) and an
// empty index used: the index only steers delivery and collection, and
// losing entries only keeps images longer.
func loadIndex(path string) (*Index, error) {
	ix := &Index{Version: indexVersion}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ix, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the image index: %w", err)
	}
	if err := json.Unmarshal(data, ix); err != nil || ix.Version > indexVersion {
		aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		_ = os.Rename(path, aside)
		flog.Warn("kubernetes image index unreadable; starting a new one", "path", path, "moved_to", aside, "err", err, "version", ix.Version)
		return &Index{Version: indexVersion}, nil
	}
	ix.Version = indexVersion
	return ix, nil
}

func saveIndex(path string, ix *Index) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the image index directory: %w", err)
	}
	data, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the image index: %w", err)
	}
	return nil
}
