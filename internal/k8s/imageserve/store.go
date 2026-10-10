package imageserve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// storeVersion is the store.json format.
const storeVersion = 1

// sweepGrace is the least time a manifest nothing references is kept: it
// covers the moment between preparing an image and a connection taking it.
// The connections' own retention (72h by default) has already passed when
// they let go (kubeimage collects a gateway image by calling Remove).
const sweepGrace = time.Minute

// Store is the gateway image cache (~/.fleet/kube/images): every manifest
// and blob fleetd serves through the gateway, content-addressed under
// blobs/sha256/<hex>, and store.json, which records what each manifest holds
// and which connections serve it. Blobs are reference-counted through their
// manifests: a blob stays while a manifest a connection serves lists it,
// even after the daemon host's docker pruned the image, and goes at the
// first sweep after the last such manifest is let go.
//
// A Store is safe for concurrent use, within one process.
type Store struct {
	// Dir is the cache directory (state.KubeImageCacheDir).
	Dir string
	// Docker is the daemon host's docker: a path, or a name looked up on
	// PATH; "" is "docker".
	Docker string
	// Now overrides the clock (tests).
	Now func() time.Time

	mu sync.Mutex
	st *storeState
	// manifests and blobs are, per connection, the manifests it serves and
	// the blobs they list: what it may pull. Rebuilt from st on every change.
	manifests, blobs map[string]map[string]bool

	// gc serializes sweeps (write) with imports (read): an import relies on
	// blobs the cache already had staying there until it records them.
	gc      sync.RWMutex
	imports flight[Prepared]
}

// storeState is store.json.
type storeState struct {
	Version int `json:"version"`
	// Images maps "<daemon-host image id>|<platform>" to the manifest
	// prepared for it, so an image is saved and compressed once.
	Images map[string]string `json:"images,omitempty"`
	// Manifests are the prepared manifests, by digest.
	Manifests map[string]*manifestRecord `json:"manifests,omitempty"`
	// DiffIDs maps an uncompressed layer's digest (its diffID) to the gzip
	// blob made from it, so a layer is compressed once and keeps one digest
	// across rebuilds (nodes then fetch only the layers that changed).
	DiffIDs map[string]string `json:"diff_ids,omitempty"`
	// Prefixes records, per connection, the registry prefix
	// ("<host[:port]>/<rid>") its image references and pull Secret were last
	// pointed at.
	Prefixes map[string]string `json:"prefixes,omitempty"`
}

// manifestRecord is one prepared manifest.
type manifestRecord struct {
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Platform  string `json:"platform"`
	// ImageID is the daemon-host image it was prepared from.
	ImageID string `json:"image_id"`
	// Blobs are the blobs it references: the config, then the layers.
	Blobs []blobRecord `json:"blobs"`
	// Refs are the connections that serve it, each with the images it was
	// delivered to that connection as (kubeimage's holders of the entry).
	Refs       map[string][]string `json:"refs,omitempty"`
	PreparedAt time.Time           `json:"prepared_at"`
	// ReleasedAt is when the last connection let go.
	ReleasedAt time.Time `json:"released_at,omitzero"`
}

type blobRecord struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Prepared is an image ready to serve.
type Prepared struct {
	// Digest is the node-platform manifest's digest: what the pod spec
	// pins.
	Digest    string
	MediaType string
	Platform  string
	// Bytes is the size of the manifest and every blob it lists (what a
	// node without any of its layers pulls).
	Bytes int64
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.Dir, "blobs", "sha256", digestHex(digest))
}

func (s *Store) statePath() string { return filepath.Join(s.Dir, "store.json") }

func (s *Store) tmpDir() string { return filepath.Join(s.Dir, "tmp") }

// state loads store.json on first use; callers hold s.mu.
func (s *Store) state() (*storeState, error) {
	if s.st != nil {
		return s.st, nil
	}
	st := &storeState{Version: storeVersion}
	data, err := os.ReadFile(s.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading the image cache index: %w", err)
	default:
		if err := json.Unmarshal(data, st); err != nil || st.Version > storeVersion {
			// The blobs are content-addressed and re-made from docker on the
			// next delivery; a lost index only costs a re-import, and the
			// sweep removes what nothing records any more.
			aside := fmt.Sprintf("%s.corrupt-%d", s.statePath(), s.now().Unix())
			_ = os.Rename(s.statePath(), aside)
			flog.Warn("gateway image cache index unreadable; starting a new one", "path", s.statePath(), "moved_to", aside, "err", err)
			st = &storeState{Version: storeVersion}
		}
	}
	st.Version = storeVersion
	s.st = st
	s.rebuildServed()
	return st, nil
}

// save writes store.json; callers hold s.mu.
func (s *Store) save() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("creating the image cache: %w", err)
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.statePath(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the image cache index: %w", err)
	}
	s.rebuildServed()
	return nil
}

// rebuildServed recomputes what each connection may pull; callers hold s.mu.
func (s *Store) rebuildServed() {
	manifests, blobs := map[string]map[string]bool{}, map[string]map[string]bool{}
	add := func(sets map[string]map[string]bool, cluster, d string) {
		if sets[cluster] == nil {
			sets[cluster] = map[string]bool{}
		}
		sets[cluster][d] = true
	}
	for d, m := range s.st.Manifests {
		for cluster, holders := range m.Refs {
			if len(holders) == 0 {
				continue
			}
			add(manifests, cluster, d)
			for _, b := range m.Blobs {
				add(blobs, cluster, b.Digest)
			}
		}
	}
	s.manifests, s.blobs = manifests, blobs
}

func imageKey(imageID, plat string) string { return imageID + "|" + plat }

// cached returns the image prepared for imageID on plat when every blob of
// it is still on disk, and touches it so a sweep keeps it for a while.
func (s *Store) cached(imageID, plat string) (Prepared, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return Prepared{}, false, err
	}
	d, ok := st.Images[imageKey(imageID, plat)]
	if !ok {
		return Prepared{}, false, nil
	}
	m := st.Manifests[d]
	if m == nil || !s.complete(d, m) {
		return Prepared{}, false, nil
	}
	if len(m.Refs) == 0 {
		m.PreparedAt = s.now()
		if err := s.save(); err != nil {
			return Prepared{}, false, err
		}
	}
	return preparedOf(d, m), true, nil
}

// complete reports whether the manifest and every blob it lists are on disk
// at their recorded sizes.
func (s *Store) complete(d string, m *manifestRecord) bool {
	if fi, err := os.Stat(s.blobPath(d)); err != nil || fi.Size() != m.Size {
		return false
	}
	for _, b := range m.Blobs {
		if fi, err := os.Stat(s.blobPath(b.Digest)); err != nil || fi.Size() != b.Size {
			return false
		}
	}
	return true
}

func preparedOf(d string, m *manifestRecord) Prepared {
	p := Prepared{Digest: d, MediaType: m.MediaType, Platform: m.Platform, Bytes: m.Size}
	for _, b := range m.Blobs {
		p.Bytes += b.Size
	}
	return p
}

// Prepare makes the daemon-host image imageID servable for plat (the
// nodes' "os/arch[/variant]"): `docker save` exports it, and its manifest
// for plat and that manifest's blobs go into the cache (compressed layers as
// they are, uncompressed ones gzip-compressed once). An image prepared
// before, whose blobs are all still cached, is not saved again. Concurrent
// calls for one image share one export.
func (s *Store) Prepare(ctx context.Context, imageID, plat string) (Prepared, error) {
	if !validDigest(imageID) {
		return Prepared{}, fmt.Errorf("image %q has no image ID", imageID)
	}
	p, err := parsePlatform(plat)
	if err != nil {
		return Prepared{}, err
	}
	if got, ok, err := s.cached(imageID, p.String()); err != nil || ok {
		return got, err
	}
	return s.imports.do(ctx, imageKey(imageID, p.String()), func(ctx context.Context) (Prepared, error) {
		if got, ok, err := s.cached(imageID, p.String()); err != nil || ok {
			return got, err
		}
		return s.saveAndImport(ctx, imageID, p)
	})
}

// recordImport records an imported manifest; callers hold s.gc for reading.
func (s *Store) recordImport(imageID string, p platform, d string, m *manifestRecord, diffIDs map[string]string) (Prepared, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return Prepared{}, err
	}
	if st.Manifests == nil {
		st.Manifests = map[string]*manifestRecord{}
	}
	if st.Images == nil {
		st.Images = map[string]string{}
	}
	if cur := st.Manifests[d]; cur != nil {
		m.Refs = cur.Refs
		if len(m.Refs) > 0 {
			m.ReleasedAt = time.Time{}
		}
	}
	m.PreparedAt = s.now()
	st.Manifests[d] = m
	st.Images[imageKey(imageID, p.String())] = d
	for diffID, gz := range diffIDs {
		if st.DiffIDs == nil {
			st.DiffIDs = map[string]string{}
		}
		st.DiffIDs[diffID] = gz
	}
	if err := s.save(); err != nil {
		return Prepared{}, err
	}
	return preparedOf(d, m), nil
}

// compressedFor returns the cached gzip blob made from an uncompressed layer
// (by its diffID), if it is still on disk.
func (s *Store) compressedFor(diffID string) (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return "", 0, false
	}
	gz, ok := st.DiffIDs[diffID]
	if !ok {
		return "", 0, false
	}
	fi, err := os.Stat(s.blobPath(gz))
	if err != nil {
		return "", 0, false
	}
	return gz, fi.Size(), true
}

// hasBlob reports whether the cache holds a blob.
func (s *Store) hasBlob(digest string) bool {
	_, err := os.Stat(s.blobPath(digest))
	return err == nil
}

// Ref makes the connection serve the manifest, for holder (the daemon-host
// image it was delivered as).
func (s *Store) Ref(clusterID, digest, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return err
	}
	m := st.Manifests[digest]
	if m == nil {
		return fmt.Errorf("image %s is not in the gateway image cache", short(digest))
	}
	if m.Refs == nil {
		m.Refs = map[string][]string{}
	}
	if slices.Contains(m.Refs[clusterID], holder) {
		return nil
	}
	m.Refs[clusterID] = append(m.Refs[clusterID], holder)
	slices.Sort(m.Refs[clusterID])
	m.ReleasedAt = time.Time{}
	return s.save()
}

// Unref lets the connection's holder go of the manifest; the connection
// stops serving it once nothing of it holds it. A manifest no connection
// serves is deleted at the next sweep after sweepGrace, with every blob no
// other manifest needs.
func (s *Store) Unref(clusterID, digest, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return err
	}
	m := st.Manifests[digest]
	if m == nil {
		return nil
	}
	i := slices.Index(m.Refs[clusterID], holder)
	if i < 0 {
		return nil
	}
	m.Refs[clusterID] = slices.Delete(m.Refs[clusterID], i, i+1)
	if len(m.Refs[clusterID]) == 0 {
		delete(m.Refs, clusterID)
	}
	if len(m.Refs) == 0 {
		m.Refs = nil
		m.ReleasedAt = s.now()
	}
	return s.save()
}

// Forget drops every reference the connection holds (it was removed) and
// its recorded prefix.
func (s *Store) Forget(clusterID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return err
	}
	for _, m := range st.Manifests {
		if _, ok := m.Refs[clusterID]; !ok {
			continue
		}
		delete(m.Refs, clusterID)
		if len(m.Refs) == 0 {
			m.Refs = nil
			m.ReleasedAt = s.now()
		}
	}
	delete(st.Prefixes, clusterID)
	return s.save()
}

// manifestFor returns the manifest a connection may pull by digest.
func (s *Store) manifestFor(clusterID, digest string) (manifestRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil || !s.manifests[clusterID][digest] {
		return manifestRecord{}, false
	}
	m := st.Manifests[digest]
	if m == nil {
		return manifestRecord{}, false
	}
	return *m, true
}

// servesBlob reports whether a connection may pull a blob: one a manifest
// it serves lists.
func (s *Store) servesBlob(clusterID, digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.state(); err != nil {
		return false
	}
	return s.blobs[clusterID][digest]
}

// servedManifests lists the manifests a connection serves.
func (s *Store) servedManifests(clusterID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.state(); err != nil {
		return nil
	}
	var out []string
	for d := range s.manifests[clusterID] {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// Serves reports whether the connection serves the manifest.
func (s *Store) Serves(clusterID, digest string) bool {
	_, ok := s.manifestFor(clusterID, digest)
	return ok
}

// Clusters lists the connections that serve at least one manifest.
func (s *Store) Clusters() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.state(); err != nil {
		return nil, err
	}
	var out []string
	for c := range s.manifests {
		out = append(out, c)
	}
	slices.Sort(out)
	return out, nil
}

// Prefix is the registry prefix a connection's references were last
// pointed at ("" when none was recorded).
func (s *Store) Prefix(clusterID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return "", err
	}
	return st.Prefixes[clusterID], nil
}

// SetPrefix records the registry prefix a connection's references now point
// at.
func (s *Store) SetPrefix(clusterID, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return err
	}
	if st.Prefixes[clusterID] == prefix {
		return nil
	}
	if st.Prefixes == nil {
		st.Prefixes = map[string]string{}
	}
	st.Prefixes[clusterID] = prefix
	return s.save()
}

// Size is the bytes the cache holds in blobs (manifests included).
func (s *Store) Size() (int64, error) {
	dir := filepath.Join(s.Dir, "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n, nil
}

// SweepResult is what one sweep removed.
type SweepResult struct {
	// Manifests are the manifests deleted (no connection served them).
	Manifests []string
	// Blobs is how many blob files were deleted, and Bytes their size.
	Blobs int
	Bytes int64
}

// Sweep deletes every manifest no connection has served for sweepGrace,
// every blob no remaining manifest lists (orphans an interrupted import left
// included), and stale import staging. It waits for running imports.
func (s *Store) Sweep() (SweepResult, error) {
	s.gc.Lock()
	defer s.gc.Unlock()
	return s.sweepLocked()
}

// TrySweep is Sweep, unless an import is running (a large `docker save` can
// take minutes): then it does nothing, and a later sweep catches up.
func (s *Store) TrySweep() (SweepResult, bool, error) {
	if !s.gc.TryLock() {
		return SweepResult{}, false, nil
	}
	defer s.gc.Unlock()
	res, err := s.sweepLocked()
	return res, true, err
}

// sweepLocked sweeps; callers hold s.gc for writing.
func (s *Store) sweepLocked() (SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.state()
	if err != nil {
		return SweepResult{}, err
	}
	var res SweepResult
	now := s.now()
	for d, m := range st.Manifests {
		if len(m.Refs) > 0 {
			continue
		}
		since := m.ReleasedAt
		if m.PreparedAt.After(since) {
			since = m.PreparedAt
		}
		if now.Sub(since) < sweepGrace {
			continue
		}
		delete(st.Manifests, d)
		res.Manifests = append(res.Manifests, d)
	}
	live := map[string]bool{}
	for d, m := range st.Manifests {
		live[digestHex(d)] = true
		for _, b := range m.Blobs {
			live[digestHex(b.Digest)] = true
		}
	}
	for k, d := range st.Images {
		if _, ok := st.Manifests[d]; !ok {
			delete(st.Images, k)
		}
	}
	for diffID, gz := range st.DiffIDs {
		if !live[digestHex(gz)] {
			delete(st.DiffIDs, diffID)
		}
	}
	dir := filepath.Join(s.Dir, "blobs", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	for _, e := range entries {
		if live[e.Name()] {
			continue
		}
		fi, _ := e.Info()
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, fmt.Errorf("removing a cached blob: %w", err)
		}
		res.Blobs++
		if fi != nil {
			res.Bytes += fi.Size()
		}
	}
	// No import runs while the sweep holds s.gc, so all staging is stale.
	_ = os.RemoveAll(s.tmpDir())
	if len(res.Manifests) > 0 || res.Blobs > 0 {
		if err := s.save(); err != nil {
			return res, err
		}
		flog.Info("gateway image cache sweep", "manifests", len(res.Manifests), "blobs", res.Blobs, "bytes", res.Bytes)
	}
	return res, nil
}

// newTempDir makes a staging directory under the cache, on its filesystem
// so finished blobs move into place by rename.
func (s *Store) newTempDir() (string, error) {
	if err := os.MkdirAll(s.tmpDir(), 0o700); err != nil {
		return "", fmt.Errorf("creating the image cache: %w", err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(s.tmpDir(), hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating an import directory: %w", err)
	}
	return dir, nil
}

// install moves a finished file into the cache as the blob digest; a blob
// already there stays (same digest, same bytes) and the file is dropped.
func (s *Store) install(path, digest string) error {
	dst := s.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("creating the image cache: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		_ = os.Remove(path)
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path, dst); err != nil {
		return fmt.Errorf("installing blob %s: %w", short(digest), err)
	}
	return nil
}
