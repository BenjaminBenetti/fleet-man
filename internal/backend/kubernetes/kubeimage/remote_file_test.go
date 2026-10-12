package kubeimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeEntry is one entry of a layer's archive.
type fakeEntry struct {
	name string
	typ  byte
	body string
	link string
}

func file(name, body string) fakeEntry { return fakeEntry{name: name, typ: tar.TypeReg, body: body} }
func dir(name string) fakeEntry        { return fakeEntry{name: name, typ: tar.TypeDir} }

// fakeLayer is a layer blob and its media type.
type fakeLayer struct {
	mediaType string
	blob      []byte
}

// layerOf archives entries as a layer, gzipped unless plain.
func layerOf(t *testing.T, plain bool, entries ...fakeEntry) fakeLayer {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser = nopWriteCloser{&buf}
	mediaType := "application/vnd.oci.image.layer.v1.tar"
	if !plain {
		w, mediaType = gzip.NewWriter(&buf), "application/vnd.oci.image.layer.v1.tar+gzip"
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return fakeLayer{mediaType: mediaType, blob: buf.Bytes()}
}

type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Close() error { return nil }

// addLayeredImage stores a single-platform image of layers (the bottom one
// first) and returns its manifest digest.
func (r *fakeRegistry) addLayeredImage(repo, tag, arch string, layers ...fakeLayer) string {
	cb, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{}})
	cfg := r.addBlob(cb)
	var ls []map[string]any
	for _, l := range layers {
		ls = append(ls, map[string]any{"mediaType": l.mediaType, "digest": r.addBlob(l.blob), "size": len(l.blob)})
	}
	mb, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfg, "size": len(cb)}, "layers": ls})
	return r.addManifest(repo, tag, mediaOCIManifest, mb)
}

const (
	baseGroup = "root:x:0:\nusers:x:100:\n"
	topGroup  = "root:x:0:\nusers:x:100:\ndocker:x:998:vscode\n"
)

// A file is read from the topmost layer that holds it, whatever its
// archive spells its path as; the layers below it are never fetched.
func TestRemoteImageFileReadsTheTopmostCopy(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.addLayeredImage("team/base", "1", "amd64",
		layerOf(t, false, dir("etc/"), file("etc/group", baseGroup), file("etc/passwd", "root:x:0:0::/root:/bin/sh\n")),
		layerOf(t, true, dir("usr/"), file("usr/bin/tool", "#!/bin/sh\n")),
		layerOf(t, false, dir("./etc/"), file("./etc/group", topGroup), dir("./var/")),
		layerOf(t, true, file("home/vscode/.bashrc", "# rc\n")),
	)
	opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
	got, err := RemoteImageFile(context.Background(), "registry.test/team/base:1", "linux/amd64", "etc/group", 1<<20, opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != topGroup {
		t.Errorf("etc/group = %q, want the top copy %q", got, topGroup)
	}
	if n := reg.count("GET /v2/team/base/blobs/"); n != 2 {
		t.Errorf("%d blobs read, want the two layers down to the one holding the file", n)
	}
}

// A layer above the file that removes it (a whiteout of the file or of a
// directory above it, an opaque directory, a directory made something else)
// leaves the image without it; the layer's own copy still counts.
func TestRemoteImageFileHonoursRemovals(t *testing.T) {
	base := layerOf(t, false, dir("etc/"), file("etc/group", baseGroup))
	for name, tc := range map[string]struct {
		top  fakeLayer
		want string
	}{
		"whiteout":                   {top: layerOf(t, false, file("etc/.wh.group", ""))},
		"directory whiteout":         {top: layerOf(t, false, file(".wh.etc", ""))},
		"opaque directory":           {top: layerOf(t, false, dir("etc/"), file("etc/.wh..wh..opq", ""), file("etc/hosts", "\n"))},
		"directory made a link":      {top: layerOf(t, false, fakeEntry{name: "etc", typ: tar.TypeSymlink, link: "/elsewhere"})},
		"opaque, with its own":       {top: layerOf(t, false, dir("etc/"), file("etc/.wh..wh..opq", ""), file("etc/group", topGroup)), want: topGroup},
		"dir whiteout, with its own": {top: layerOf(t, false, file(".wh.etc", ""), dir("etc/"), file("etc/group", topGroup)), want: topGroup},
	} {
		t.Run(name, func(t *testing.T) {
			reg := newFakeRegistry(t)
			reg.addLayeredImage("team/base", "1", "amd64", base, tc.top)
			opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
			got, err := RemoteImageFile(context.Background(), "registry.test/team/base:1", "", "/etc/group", 1<<20, opts)
			if tc.want == "" {
				if !errors.Is(err, ErrNoImageFile) || got != nil {
					t.Errorf("RemoteImageFile = %q, %v; want ErrNoImageFile", got, err)
				}
				if n := reg.count("GET /v2/team/base/blobs/"); n != 1 {
					t.Errorf("%d blobs read, want only the layer that removes the file", n)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("RemoteImageFile = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// An image that never held the file, or holds something else at its path,
// or a layer whose bytes are not its digest's, gives no file.
func TestRemoteImageFileRefusals(t *testing.T) {
	reg := newFakeRegistry(t)
	opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
	read := func(tag string) ([]byte, error) {
		return RemoteImageFile(context.Background(), "registry.test/team/base:"+tag, "linux/amd64", "etc/group", 64, opts)
	}

	reg.addLayeredImage("team/base", "none", "amd64", layerOf(t, false, file("etc/passwd", "\n")), layerOf(t, true, file("bin/sh", "")))
	if _, err := read("none"); !errors.Is(err, ErrNoImageFile) {
		t.Errorf("an image without the file: %v", err)
	}
	reg.addLayeredImage("team/base", "link", "amd64", layerOf(t, false, fakeEntry{name: "etc/group", typ: tar.TypeSymlink, link: "/host/etc/group"}))
	if _, err := read("link"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a link at the file's path: %v", err)
	}
	reg.addLayeredImage("team/base", "big", "amd64", layerOf(t, false, file("etc/group", strings.Repeat("g", 65))))
	if _, err := read("big"); err == nil || !strings.Contains(err.Error(), "larger than 64 bytes") {
		t.Errorf("a file over the limit: %v", err)
	}
	reg.addLayeredImage("team/base", "zstd", "amd64", fakeLayer{mediaType: "application/vnd.oci.image.layer.v1.tar+zstd", blob: []byte("zstd")})
	if _, err := read("zstd"); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("a zstd layer: %v", err)
	}
	// The registry answers a layer's digest with other bytes.
	good := layerOf(t, false, file("etc/group", baseGroup))
	reg.addLayeredImage("team/base", "tampered", "amd64", good)
	reg.mu.Lock()
	reg.blobs[sha256Digest(good.blob)] = layerOf(t, false, file("etc/group", "root:x:0:\nusers:x:0:\n")).blob
	reg.mu.Unlock()
	if got, err := read("tampered"); err == nil || !strings.Contains(err.Error(), "has digest") || got != nil {
		t.Errorf("a layer that is not its digest's: %q, %v", got, err)
	}
}

// An index's image for the platform asked for is the one read.
func TestRemoteImageFilePicksThePlatform(t *testing.T) {
	reg := newFakeRegistry(t)
	amd := reg.addLayeredImage("team/base", "", "amd64", layerOf(t, false, file("etc/group", baseGroup)))
	arm := reg.addLayeredImage("team/base", "", "arm64", layerOf(t, false, file("etc/group", topGroup)))
	idx := reg.addIndex("team/base", "1", map[string]string{"linux/amd64": amd, "linux/arm64": arm})
	opts := RemoteOptions{Credentials: staticCredentials{}, Transport: reg.transport()}
	for ref, want := range map[string]string{"registry.test/team/base:1": topGroup, "registry.test/team/base@" + idx: topGroup} {
		got, err := RemoteImageFile(context.Background(), ref, "linux/arm64", "etc/group", 1<<20, opts)
		if err != nil || string(got) != want {
			t.Errorf("%s for arm64: %q, %v", ref, got, err)
		}
	}
	if got, err := RemoteImageFile(context.Background(), "registry.test/team/base:1", "linux/amd64", "etc/group", 1<<20, opts); err != nil || string(got) != baseGroup {
		t.Errorf("for amd64: %q, %v", got, err)
	}
}
