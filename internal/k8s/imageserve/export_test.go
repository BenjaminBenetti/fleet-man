package imageserve

import "testing"

// export_test.go hands the in-package test doubles to the end-to-end tests
// (package imageserve_test), which drive the image server through a real
// gateway and fleetd's tunnel client.

// TestArchive is a containerd-store `docker save` archive of one image.
type TestArchive struct {
	Archive []byte
	// ImageID is the image (index) ID the archive is saved as.
	ImageID string
	// Manifest is the node-platform manifest's digest, Config its config's,
	// Layers the layers as served.
	Manifest, Config string
	ManifestBytes    []byte
	Layers           [][]byte
}

// NewTestArchive builds a linux/<arch> image with one gzip layer per file
// set, saved as docker's containerd store does.
func NewTestArchive(t testing.TB, arch string, layers ...map[string]string) TestArchive {
	t.Helper()
	var tars [][]byte
	for _, files := range layers {
		tars = append(tars, layerTar(t, files))
	}
	img := newTestImage(t, arch, "", true, tars...)
	archive, id := containerdArchive(t, []testImage{img})
	return TestArchive{Archive: archive, ImageID: id, Manifest: img.manifestDigest, Config: img.configDigest, ManifestBytes: img.manifest, Layers: img.layers}
}

// FakeDocker is the scripted docker whose `docker save` answers archives.
type FakeDocker struct{ f *fakeDocker }

func NewFakeDocker(t testing.TB) FakeDocker { return FakeDocker{newFakeDocker(t)} }

func (d FakeDocker) Path() string                          { return d.f.path() }
func (d FakeDocker) Add(t testing.TB, id string, a []byte) { d.f.add(t, id, a) }
func (d FakeDocker) Saves() int                            { return d.f.saves() }
func Sha256Digest(b []byte) string                         { return sha256Digest(b) }
func RandomBytes(n int, seed uint64) []byte                { return randomBytes(n, seed) }
func (u *Uplink) SetEstimateForTest(bytesPerSecond float64) {
	u.mu.Lock()
	u.estimate = bytesPerSecond
	u.mu.Unlock()
}
