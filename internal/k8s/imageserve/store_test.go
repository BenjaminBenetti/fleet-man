package imageserve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// importImage imports a compressed (containerd-store) image into s and
// returns the prepared result with the test image.
func importImage(t testing.TB, s *Store, files map[string]string, shared ...[]byte) (Prepared, testImage) {
	t.Helper()
	tars := append(append([][]byte(nil), shared...), layerTar(t, files))
	img := newTestImage(t, "amd64", "", true, tars...)
	archive, id := containerdArchive(t, []testImage{img})
	p, err := s.Import(context.Background(), bytes.NewReader(archive), id, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	return p, img
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestStoreRefcountsBlobsThroughManifests(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := &Store{Dir: t.TempDir(), Now: clock.now}
	base := layerTar(t, map[string]string{"base": "shared"})
	p1, img1 := importImage(t, s, map[string]string{"app": "one"}, base)
	p2, img2 := importImage(t, s, map[string]string{"app": "two"}, base)
	shared := sha256Digest(img1.layers[0])
	if shared != sha256Digest(img2.layers[0]) {
		t.Fatal("test images do not share their base layer")
	}

	mustRef := func(c, d, h string) {
		t.Helper()
		if err := s.Ref(c, d, h); err != nil {
			t.Fatal(err)
		}
	}
	mustRef("c1", p1.Digest, "img1")
	mustRef("c2", p1.Digest, "img1")
	mustRef("c2", p2.Digest, "img2")

	// Each connection serves only its own manifests and their blobs.
	if !s.Serves("c1", p1.Digest) || s.Serves("c1", p2.Digest) || !s.Serves("c2", p2.Digest) {
		t.Fatal("per-connection image sets are wrong")
	}
	if !s.servesBlob("c1", shared) || s.servesBlob("c1", sha256Digest(img2.layers[1])) || s.servesBlob("c1", p1.Digest) {
		t.Fatal("per-connection blob sets are wrong (a manifest is not a blob)")
	}

	// Unreferenced for less than the grace: kept.
	clock.advance(time.Hour)
	if err := s.Unref("c2", p2.Digest, "img2"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Sweep(); err != nil || len(res.Manifests) != 0 {
		t.Fatalf("sweep within grace: %+v, %v", res, err)
	}
	clock.advance(2 * sweepGrace)
	res, err := s.Sweep()
	if err != nil || len(res.Manifests) != 1 || res.Manifests[0] != p2.Digest {
		t.Fatalf("sweep: %+v, %v", res, err)
	}
	// img2's own top layer and config went; the shared base stays for p1.
	for _, d := range []string{sha256Digest(img2.layers[1]), img2.configDigest, p2.Digest} {
		if s.hasBlob(d) {
			t.Errorf("blob %s outlived its last manifest", short(d))
		}
	}
	for _, d := range []string{shared, sha256Digest(img1.layers[1]), img1.configDigest, p1.Digest} {
		if !s.hasBlob(d) {
			t.Errorf("blob %s of a served manifest was deleted", short(d))
		}
	}

	// p1 stays while either connection serves it, even with one holder gone.
	mustRef("c1", p1.Digest, "img1-rebuild")
	_ = s.Unref("c1", p1.Digest, "img1")
	_ = s.Forget("c2")
	clock.advance(time.Hour)
	if res, _ := s.Sweep(); len(res.Manifests) != 0 {
		t.Fatalf("a manifest c1 still holds was swept: %+v", res)
	}
	_ = s.Unref("c1", p1.Digest, "img1-rebuild")
	clock.advance(time.Hour)
	if res, _ := s.Sweep(); len(res.Manifests) != 1 {
		t.Fatalf("the last manifest was not swept: %+v", res)
	}
	if n, _ := s.Size(); n != 0 {
		t.Fatalf("the cache still holds %d bytes", n)
	}
}

func TestStoreSweepRemovesOrphansAndStaging(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := &Store{Dir: t.TempDir(), Now: clock.now}
	p, _ := importImage(t, s, map[string]string{"a": "1"})
	_ = s.Ref("c", p.Digest, "h")
	orphan := s.blobPath(sha256Digest([]byte("orphan")))
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.tmpDir(), "dead")
	_ = os.MkdirAll(stale, 0o700)
	res, err := s.Sweep()
	if err != nil || res.Blobs != 1 {
		t.Fatalf("sweep: %+v, %v", res, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("an orphan blob survived")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staging survived")
	}
	if !s.Serves("c", p.Digest) {
		t.Error("the sweep dropped a served manifest")
	}
}

func TestStorePersistsAndRecoversFromACorruptIndex(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	p, _ := importImage(t, s, map[string]string{"a": "1"})
	_ = s.Ref("c", p.Digest, "h")
	_ = s.SetPrefix("c", "gw.example.com/rid")
	fi, err := os.Stat(s.statePath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store.json: %v, %v", fi, err)
	}

	reopened := &Store{Dir: dir}
	if !reopened.Serves("c", p.Digest) {
		t.Fatal("a reopened store forgot what it serves")
	}
	if got, _ := reopened.Prefix("c"); got != "gw.example.com/rid" {
		t.Fatalf("prefix = %q", got)
	}

	if err := os.WriteFile(s.statePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := &Store{Dir: dir}
	if broken.Serves("c", p.Digest) {
		t.Fatal("a corrupt index still serves")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "store.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt index not moved aside: %v", matches)
	}
}

func TestStoreRefNeedsAPreparedManifest(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ref("c", sha256Digest([]byte("x")), "h"); err == nil || !strings.Contains(err.Error(), "not in the gateway image cache") {
		t.Fatalf("Ref of an unknown manifest: %v", err)
	}
	// Unref and Forget of what is not there are no-ops.
	if err := s.Unref("c", sha256Digest([]byte("x")), "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("c"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCachedTouchesAnUnreferencedManifest(t *testing.T) {
	// Preparing an image again restarts its grace, so a sweep between the
	// Prepare and the Ref cannot take it.
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	docker := newFakeDocker(t)
	s := &Store{Dir: t.TempDir(), Now: clock.now, Docker: docker.path()}
	img := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"a": "1"}))
	archive, id := containerdArchive(t, []testImage{img})
	docker.add(t, id, archive)
	if _, err := s.Prepare(context.Background(), id, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Hour)
	p, err := s.Prepare(context.Background(), id, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Sweep(); len(res.Manifests) != 0 {
		t.Fatal("a just-prepared manifest was swept")
	}
	if err := s.Ref("c", p.Digest, id); err != nil {
		t.Fatal(err)
	}
}
