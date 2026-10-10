package imageserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func newTestStore(t testing.TB) *Store {
	t.Helper()
	return &Store{Dir: t.TempDir()}
}

// readBlob reads a cached blob, checking it matches its digest.
func readBlob(t testing.TB, s *Store, d string) []byte {
	t.Helper()
	b, err := os.ReadFile(s.blobPath(d))
	if err != nil {
		t.Fatalf("blob %s: %v", short(d), err)
	}
	if got := sha256Digest(b); got != d {
		t.Fatalf("blob %s holds %s", short(d), short(got))
	}
	return b
}

// servedManifest reads a prepared manifest back from the cache.
func servedManifest(t testing.TB, s *Store, d string) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(readBlob(t, s, d), &m); err != nil {
		t.Fatalf("manifest %s: %v", short(d), err)
	}
	return m
}

// checkLayers verifies every layer of a served manifest is cached, gzip or
// as stored, and decompresses to the config's diff_ids.
func checkLayers(t testing.TB, s *Store, m manifest) {
	t.Helper()
	var cfg struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(readBlob(t, s, m.Config.Digest), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		t.Fatalf("%d layers for %d diff_ids", len(m.Layers), len(cfg.RootFS.DiffIDs))
	}
	for i, l := range m.Layers {
		b := readBlob(t, s, l.Digest)
		if int64(len(b)) != l.Size {
			t.Errorf("layer %d is %d bytes, its descriptor says %d", i, len(b), l.Size)
		}
		if !isCompressedLayer(l.MediaType) {
			t.Errorf("layer %d is served as %s, want a compressed type", i, l.MediaType)
		}
		if got := sha256Digest(gunzip(t, b)); got != cfg.RootFS.DiffIDs[i] {
			t.Errorf("layer %d decompresses to %s, want diff_id %s", i, short(got), short(cfg.RootFS.DiffIDs[i]))
		}
	}
}

func TestImportContainerdStoreServesTheNodePlatformManifestAsIs(t *testing.T) {
	s := newTestStore(t)
	amd := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"a": "base"}), layerTar(t, map[string]string{"b": "top"}))
	arm := newTestImage(t, "arm64", "v8", true, layerTar(t, map[string]string{"a": "arm base"}))
	archive, id := containerdArchive(t, []testImage{amd, arm}, "linux/s390x")

	for plat, want := range map[string]testImage{"linux/amd64": amd, "linux/arm64": arm, "linux/arm64/v8": arm} {
		p, err := s.Import(context.Background(), bytes.NewReader(archive), id, plat)
		if err != nil {
			t.Fatalf("%s: %v", plat, err)
		}
		// The archive's own manifest, byte for byte: its digest is the
		// platform manifest's, never the image (index) ID.
		if p.Digest != want.manifestDigest || p.Digest == id {
			t.Fatalf("%s: digest %s, want the platform manifest %s", plat, short(p.Digest), short(want.manifestDigest))
		}
		if !bytes.Equal(readBlob(t, s, p.Digest), want.manifest) {
			t.Fatalf("%s: the manifest was rewritten", plat)
		}
		if p.MediaType != mediaOCIManifest {
			t.Errorf("%s: media type %q", plat, p.MediaType)
		}
		for i, l := range want.layers {
			if !bytes.Equal(readBlob(t, s, sha256Digest(l)), l) {
				t.Errorf("%s: layer %d not served byte for byte", plat, i)
			}
		}
		wantBytes := int64(len(want.manifest) + len(want.config))
		for _, l := range want.layers {
			wantBytes += int64(len(l))
		}
		if p.Bytes != wantBytes {
			t.Errorf("%s: Bytes %d, want %d", plat, p.Bytes, wantBytes)
		}
	}

	_, err := s.Import(context.Background(), bytes.NewReader(archive), id, "linux/ppc64le")
	if err == nil || !strings.Contains(err.Error(), "linux/amd64") || !strings.Contains(err.Error(), "linux/ppc64le") {
		t.Fatalf("a platform the host lacks: %v", err)
	}
}

func TestImportClassicStoreCompressesOnceByDiffID(t *testing.T) {
	s := newTestStore(t)
	base := layerTar(t, map[string]string{"etc/os-release": "fleet test base"})
	img1 := newTestImage(t, "amd64", "", false, base, layerTar(t, map[string]string{"app": "v1"}))
	img2 := newTestImage(t, "amd64", "", false, base, layerTar(t, map[string]string{"app": "v2"}))

	p1, err := s.Import(context.Background(), bytes.NewReader(classicArchive(t, img1)), img1.configDigest, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if p1.Digest == img1.manifestDigest {
		t.Fatal("uncompressed layers must get a new manifest")
	}
	m1 := servedManifest(t, s, p1.Digest)
	if m1.MediaType != mediaOCIManifest || m1.Config.Digest != img1.configDigest {
		t.Fatalf("manifest %+v", m1)
	}
	checkLayers(t, s, m1)
	if m1.Layers[0].MediaType != mediaOCILayerGzip {
		t.Errorf("layer type %q", m1.Layers[0].MediaType)
	}

	// The rebuild shares the base layer: the same compressed blob, so a node
	// that has it fetches only the new top layer.
	p2, err := s.Import(context.Background(), bytes.NewReader(classicArchive(t, img2)), img2.configDigest, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	m2 := servedManifest(t, s, p2.Digest)
	checkLayers(t, s, m2)
	if m2.Layers[0].Digest != m1.Layers[0].Digest {
		t.Fatalf("the shared base layer got two digests: %s, %s", short(m1.Layers[0].Digest), short(m2.Layers[0].Digest))
	}
	if m2.Layers[1].Digest == m1.Layers[1].Digest {
		t.Fatal("different top layers got one digest")
	}
	s.mu.Lock()
	gz := s.st.DiffIDs[sha256Digest(base)]
	s.mu.Unlock()
	if gz != m1.Layers[0].Digest {
		t.Fatalf("diff_ids[base] = %s, want %s", short(gz), short(m1.Layers[0].Digest))
	}

	// The manifest is written once: importing the same image again gives the
	// same digest.
	again, err := s.Import(context.Background(), bytes.NewReader(classicArchive(t, img1)), img1.configDigest, "linux/amd64")
	if err != nil || again.Digest != p1.Digest {
		t.Fatalf("re-import: %+v, %v (want %s)", again, err, short(p1.Digest))
	}
}

func TestImportClassicStoreRecompressesToTheSameDigest(t *testing.T) {
	// A lost cache index (or a swept gzip blob) compresses the layer again;
	// Go's gzip output has no timestamp, so its digest is unchanged.
	img := newTestImage(t, "amd64", "", false, layerTar(t, map[string]string{"x": strings.Repeat("y", 10000)}))
	var digests []string
	for range 2 {
		s := newTestStore(t)
		p, err := s.Import(context.Background(), bytes.NewReader(classicArchive(t, img)), img.configDigest, "linux/amd64")
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, p.Digest)
	}
	if digests[0] != digests[1] {
		t.Fatalf("two compressions of one image gave %s and %s", short(digests[0]), short(digests[1]))
	}
}

func TestImportLegacyManifestJSON(t *testing.T) {
	s := newTestStore(t)
	img := newTestImage(t, "amd64", "", false, layerTar(t, map[string]string{"a": "1"}), layerTar(t, map[string]string{"b": "2"}))
	p, err := s.Import(context.Background(), bytes.NewReader(legacyArchive(t, img, true)), img.configDigest, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	m := servedManifest(t, s, p.Digest)
	if m.MediaType != mediaOCIManifest || m.Config.MediaType != mediaOCIConfig || m.Config.Digest != img.configDigest {
		t.Fatalf("manifest %+v", m)
	}
	if len(m.Layers) != 3 {
		t.Fatalf("%d layers, want 3 (the duplicate symlink included)", len(m.Layers))
	}
	if m.Layers[2].Digest != m.Layers[0].Digest {
		t.Fatal("a symlinked duplicate layer must be the same blob")
	}
	for _, l := range m.Layers {
		if got := sha256Digest(gunzip(t, readBlob(t, s, l.Digest))); got != img.diffIDs[0] && got != img.diffIDs[1] {
			t.Errorf("layer decompresses to %s", short(got))
		}
	}

	if _, err := s.Import(context.Background(), bytes.NewReader(legacyArchive(t, img, false)), img.configDigest, "linux/arm64"); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("a single-platform image for another platform: %v", err)
	}
}

func TestImportRefusesBadArchives(t *testing.T) {
	img := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"a": "1"}))
	for name, archive := range map[string][]byte{
		"digest mismatch": func() []byte {
			a := newArchive(t)
			a.file("blobs/sha256/"+digestHex(sha256Digest([]byte("other"))), []byte("not other"))
			a.file("index.json", []byte(`{"manifests":[]}`))
			return a.bytes()
		}(),
		"unsafe path": func() []byte {
			a := newArchive(t)
			a.file("../escape", []byte("x"))
			return a.bytes()
		}(),
		"no layout": func() []byte {
			a := newArchive(t)
			a.image(img)
			return a.bytes()
		}(),
		"missing layer": func() []byte {
			a := newArchive(t)
			a.blob(img.config)
			a.blob(img.manifest)
			a.file("index.json", mustJSON(t, map[string]any{"manifests": []any{map[string]any{"mediaType": mediaOCIManifest, "digest": img.manifestDigest, "size": len(img.manifest)}}}))
			return a.bytes()
		}(),
		"not a tar": []byte("definitely not a tar archive, it is too short"),
	} {
		s := newTestStore(t)
		if _, err := s.Import(context.Background(), bytes.NewReader(archive), img.manifestDigest, "linux/amd64"); err == nil {
			t.Errorf("%s: imported", name)
		}
		if entries, _ := os.ReadDir(s.tmpDir()); len(entries) != 0 {
			t.Errorf("%s: staging left behind", name)
		}
	}
}

func TestImportSkipsBlobsTheCacheHas(t *testing.T) {
	s := newTestStore(t)
	img := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"a": strings.Repeat("big", 1000)}))
	archive, id := containerdArchive(t, []testImage{img})
	if _, err := s.Import(context.Background(), bytes.NewReader(archive), id, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	// Corrupt nothing, but make the cached layer distinguishable: a second
	// import must keep the cached file, not write the archive's copy.
	layer := s.blobPath(sha256Digest(img.layers[0]))
	before, _ := os.Stat(layer)
	if _, err := s.Import(context.Background(), bytes.NewReader(archive), id, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(layer)
	if !os.SameFile(before, after) {
		t.Fatal("a cached blob was written again")
	}
}

func TestPrepareSavesOnceAndSurvivesAPrunedImage(t *testing.T) {
	docker := newFakeDocker(t)
	s := &Store{Dir: t.TempDir(), Docker: docker.path()}
	img := newTestImage(t, "amd64", "", false, layerTar(t, map[string]string{"a": "1"}))
	docker.add(t, img.configDigest, classicArchive(t, img))

	p, err := s.Prepare(context.Background(), img.configDigest, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ref("c1", p.Digest, img.configDigest); err != nil {
		t.Fatal(err)
	}
	// Prepared once: later deliveries (even after the host pruned the
	// image) use the cache.
	docker.remove(img.configDigest)
	for range 3 {
		again, err := s.Prepare(context.Background(), img.configDigest, "linux/amd64")
		if err != nil || again != p {
			t.Fatalf("Prepare again: %+v, %v", again, err)
		}
	}
	if n := docker.saves(); n != 1 {
		t.Fatalf("docker save ran %d times, want 1", n)
	}

	// A cache that lost a blob saves again; with the image gone that is
	// ImageGoneError.
	_ = os.Remove(s.blobPath(servedManifest(t, s, p.Digest).Layers[0].Digest))
	_, err = s.Prepare(context.Background(), img.configDigest, "linux/amd64")
	var gone *ImageGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("Prepare of a pruned image with a damaged cache: %v", err)
	}
}

func TestPrepareSharesOneSave(t *testing.T) {
	docker := newFakeDocker(t)
	s := &Store{Dir: t.TempDir(), Docker: docker.path()}
	img := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"a": "1"}))
	archive, id := containerdArchive(t, []testImage{img})
	docker.add(t, id, archive)
	errs := make(chan error, 6)
	for range 6 {
		go func() {
			_, err := s.Prepare(context.Background(), id, "linux/amd64")
			errs <- err
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := docker.saves(); n != 1 {
		t.Fatalf("6 concurrent deliveries ran docker save %d times, want 1", n)
	}
}

func TestPrepareValidates(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Prepare(context.Background(), "nope", "linux/amd64"); err == nil {
		t.Error("an invalid image ID was accepted")
	}
	if _, err := s.Prepare(context.Background(), sha256Digest([]byte("x")), "amd64"); err == nil {
		t.Error("an invalid platform was accepted")
	}
}

func TestPlatformMatching(t *testing.T) {
	for _, c := range []struct {
		want, os, arch, variant string
		ok                      bool
	}{
		{"linux/amd64", "linux", "amd64", "", true},
		{"linux/x86_64", "linux", "amd64", "", true},
		{"linux/aarch64", "linux", "arm64", "v8", true},
		{"linux/arm64", "linux", "arm64", "", true},
		{"linux/arm64/v8", "linux", "arm64", "", true},
		{"linux/arm/v7", "linux", "arm", "v6", false},
		{"linux/arm", "linux", "arm", "v7", true},
		{"linux/amd64", "linux", "arm64", "", false},
		{"linux/amd64", "windows", "amd64", "", false},
	} {
		p, err := parsePlatform(c.want)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.matches(c.os, c.arch, c.variant); got != c.ok {
			t.Errorf("%s matches %s/%s/%s = %v", c.want, c.os, c.arch, c.variant, got)
		}
	}
}
