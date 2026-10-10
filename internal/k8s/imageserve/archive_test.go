package imageserve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archive_test.go builds `docker save` archives in the three layouts the
// image server reads, and a scripted docker that answers `docker save` with
// them.

// layerTar is an uncompressed layer holding files.
func layerTar(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gzipBytes(t testing.TB, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func gunzip(t testing.TB, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testImage is one platform image as an archive holds it.
type testImage struct {
	arch, variant  string
	config         []byte
	configDigest   string
	diffIDs        []string
	layers         [][]byte // as stored (compressed or not)
	layerTypes     []string
	manifest       []byte
	manifestDigest string
}

// newTestImage builds an image for linux/arch[/variant] from uncompressed
// layer tars, storing them gzip-compressed (compressed) or as they are.
func newTestImage(t testing.TB, arch, variant string, compressed bool, tars ...[]byte) testImage {
	t.Helper()
	img := testImage{arch: arch, variant: variant}
	var descs []map[string]any
	for _, lt := range tars {
		img.diffIDs = append(img.diffIDs, sha256Digest(lt))
		stored, mt := lt, mediaOCILayer
		if compressed {
			stored, mt = gzipBytes(t, lt), mediaOCILayerGzip
		}
		img.layers = append(img.layers, stored)
		img.layerTypes = append(img.layerTypes, mt)
		descs = append(descs, map[string]any{"mediaType": mt, "digest": sha256Digest(stored), "size": len(stored)})
	}
	cfg := map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{"Cmd": []string{"sh"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": img.diffIDs}}
	if variant != "" {
		cfg["variant"] = variant
	}
	img.config = mustJSON(t, cfg)
	img.configDigest = sha256Digest(img.config)
	if descs == nil {
		descs = []map[string]any{}
	}
	img.manifest = mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaOCIManifest,
		"config":        map[string]any{"mediaType": mediaOCIConfig, "digest": img.configDigest, "size": len(img.config)},
		"layers":        descs,
	})
	img.manifestDigest = sha256Digest(img.manifest)
	return img
}

// archiveWriter writes a tar of named files.
type archiveWriter struct {
	t  testing.TB
	b  bytes.Buffer
	tw *tar.Writer
}

func newArchive(t testing.TB) *archiveWriter {
	a := &archiveWriter{t: t}
	a.tw = tar.NewWriter(&a.b)
	return a
}

func (a *archiveWriter) file(name string, body []byte) {
	a.t.Helper()
	if err := a.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.tw.Write(body); err != nil {
		a.t.Fatal(err)
	}
}

func (a *archiveWriter) symlink(name, target string) {
	a.t.Helper()
	if err := a.tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, ModTime: time.Unix(0, 0)}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *archiveWriter) blob(body []byte) string {
	d := sha256Digest(body)
	a.file("blobs/sha256/"+digestHex(d), body)
	return d
}

func (a *archiveWriter) bytes() []byte {
	a.t.Helper()
	if err := a.tw.Close(); err != nil {
		a.t.Fatal(err)
	}
	return a.b.Bytes()
}

func (a *archiveWriter) image(img testImage) {
	a.blob(img.config)
	for _, l := range img.layers {
		a.blob(l)
	}
	a.blob(img.manifest)
}

// containerdArchive is docker's containerd-store save: index.json names the
// image's own index, which lists the present images, platforms the host
// lacks (missing, "os/arch") and an attestation manifest. It returns the
// archive and the image index's digest (the image ID).
func containerdArchive(t testing.TB, imgs []testImage, missing ...string) ([]byte, string) {
	t.Helper()
	a := newArchive(t)
	var entries []map[string]any
	for _, img := range imgs {
		a.image(img)
		p := map[string]any{"os": "linux", "architecture": img.arch}
		if img.variant != "" {
			p["variant"] = img.variant
		}
		entries = append(entries, map[string]any{"mediaType": mediaOCIManifest, "digest": img.manifestDigest, "size": len(img.manifest), "platform": p})
	}
	for _, m := range missing {
		os, arch, _ := strings.Cut(m, "/")
		entries = append(entries, map[string]any{"mediaType": mediaOCIManifest, "digest": sha256Digest([]byte(m)), "size": 500, "platform": map[string]any{"os": os, "architecture": arch}})
	}
	// buildkit's attestation manifest: present, but not an image.
	attCfg := a.blob([]byte(`{"architecture":"unknown","os":"unknown","rootfs":{"type":"layers","diff_ids":[]}}`))
	att := mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaOCIManifest,
		"config": map[string]any{"mediaType": mediaOCIConfig, "digest": attCfg, "size": 83}, "layers": []any{}})
	attD := a.blob(att)
	entries = append(entries, map[string]any{"mediaType": mediaOCIManifest, "digest": attD, "size": len(att),
		"platform":    map[string]any{"os": "unknown", "architecture": "unknown"},
		"annotations": map[string]string{attestationAnnotation: "attestation-manifest"}})
	ix := mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex, "manifests": entries})
	ixD := a.blob(ix)
	a.file("index.json", mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex,
		"manifests": []any{map[string]any{"mediaType": mediaOCIIndex, "digest": ixD, "size": len(ix)}}}))
	a.file("oci-layout", []byte(`{"imageLayoutVersion": "1.0.0"}`))
	return a.bytes(), ixD
}

// classicArchive is docker's classic-store save (Docker 25+): index.json
// names the one manifest, with uncompressed layers. The image ID is the
// config digest.
func classicArchive(t testing.TB, img testImage) []byte {
	t.Helper()
	a := newArchive(t)
	a.image(img)
	a.file("index.json", mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex,
		"manifests": []any{map[string]any{"mediaType": mediaOCIManifest, "digest": img.manifestDigest, "size": len(img.manifest)}}}))
	a.file("manifest.json", mustJSON(t, []any{map[string]any{"Config": "blobs/sha256/" + digestHex(img.configDigest), "Layers": []string{}}}))
	a.file("oci-layout", []byte(`{"imageLayoutVersion": "1.0.0"}`))
	return a.bytes()
}

// legacyArchive is an older docker's save: manifest.json only, layers as
// <id>/layer.tar; a layer listed twice is a symlink to the first.
func legacyArchive(t testing.TB, img testImage, duplicateFirst bool) []byte {
	t.Helper()
	a := newArchive(t)
	cfgName := digestHex(img.configDigest) + ".json"
	a.file(cfgName, img.config)
	var layers []string
	for i, l := range img.layers {
		dir := strings.Repeat(string(rune('a'+i)), 64)
		a.file(dir+"/layer.tar", l)
		a.file(dir+"/VERSION", []byte("1.0"))
		layers = append(layers, dir+"/layer.tar")
	}
	if duplicateFirst && len(layers) > 0 {
		dir := strings.Repeat("z", 64)
		a.symlink(dir+"/layer.tar", "../"+layers[0])
		layers = append(layers, dir+"/layer.tar")
	}
	a.file("manifest.json", mustJSON(t, []any{map[string]any{"Config": cfgName, "RepoTags": []string{"t:1"}, "Layers": layers}}))
	return a.bytes()
}

// fakeDocker is a scripted docker: `docker save <id>` writes
// <dir>/saves/<id hex>.tar (or fails "No such image"), and every call is
// journaled to <dir>/docker.log.
type fakeDocker struct {
	dir string
}

const fakeDockerScript = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/docker.log"
case "$1" in
image)
	# image inspect --format {{.Id}} -- <id>
	for a; do id=$a; done
	if [ ! -f "$d/saves/${id#sha256:}.tar" ]; then
		echo "Error response from daemon: No such image: $id" >&2; exit 1
	fi
	echo "$id" ;;
save)
	f="$d/saves/${2#sha256:}.tar"
	if [ ! -f "$f" ]; then
		echo "Error response from daemon: No such image: $2" >&2; exit 1
	fi
	cat "$f" ;;
*)
	echo "unexpected docker $*" >&2; exit 2 ;;
esac
`

func newFakeDocker(t testing.TB) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "saves"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeDocker{dir: dir}
}

func (f *fakeDocker) path() string { return filepath.Join(f.dir, "docker") }

// add makes `docker save <id>` answer archive.
func (f *fakeDocker) add(t testing.TB, id string, archive []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "saves", digestHex(id)+".tar"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
}

// remove forgets an image (the host pruned it).
func (f *fakeDocker) remove(id string) {
	_ = os.Remove(filepath.Join(f.dir, "saves", digestHex(id)+".tar"))
}

// saves counts the `docker save` calls.
func (f *fakeDocker) saves() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "docker.log"))
	return strings.Count(string(b), "save ")
}
