package kubeimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ErrNoImageFile is a file an image's root does not hold.
var ErrNoImageFile = errors.New("the image has no such file")

// RemoteImageFile reads one file of an image from its registry, for the
// platform RemoteImageConfig reads the config of, without pulling the image:
// name is a path in the image's root ("etc/group"), its content a regular
// file of at most limit bytes. The layers are read from the top one down,
// until one holds the file or removes it (a whiteout of it or of a directory
// above it, an opaque directory above it, a directory above it made
// anything else); ErrNoImageFile when no layer holds it. A layer is read
// whole, and nothing of it is believed before its digest is checked.
func RemoteImageFile(ctx context.Context, ref, platform, name string, limit int64, opts RemoteOptions) ([]byte, error) {
	name = path.Clean(strings.TrimPrefix(name, "/"))
	if name == "." || strings.HasPrefix(name, "../") {
		return nil, fmt.Errorf("%q is not a file in an image", name)
	}
	pm, err := readPlatformManifest(ctx, ref, platform, opts)
	if err != nil {
		return nil, err
	}
	var m struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(pm.body, &m); err != nil {
		return nil, fmt.Errorf("%s: unreadable manifest: %w", ref, err)
	}
	for i := len(m.Layers) - 1; i >= 0; i-- {
		l := m.Layers[i]
		if !digestPattern.MatchString(l.Digest) {
			return nil, fmt.Errorf("%s: the manifest's layer %d has no digest", ref, i)
		}
		data, found, removed, err := pm.c.layerFile(ctx, pm.ref.Path, l.Digest, l.MediaType, name, limit)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s: layer %s: %w", ref, l.Digest, err)
		case found:
			return data, nil
		case removed:
			return nil, fmt.Errorf("/%s: %w", name, ErrNoImageFile)
		}
	}
	return nil, fmt.Errorf("/%s: %w", name, ErrNoImageFile)
}

// layerFile reads layer digest of repoPath for file name: its content when
// the layer holds it (found), or whether the layer removes what the layers
// below hold of it. A layer's own whiteouts apply to the layers below, never
// to the file the layer itself holds.
func (c *registryClient) layerFile(ctx context.Context, repoPath, digest, mediaType, name string, limit int64) (data []byte, found, removed bool, err error) {
	blob, verify, err := c.openBlob(ctx, repoPath, digest)
	if err != nil {
		return nil, false, false, err
	}
	defer blob.Close()
	var stream io.Reader = blob
	switch {
	case strings.HasSuffix(mediaType, "+gzip"), strings.HasSuffix(mediaType, ".tar.gzip"):
		gz, err := gzip.NewReader(blob)
		if err != nil {
			return nil, false, false, err
		}
		stream = gz
	case strings.HasSuffix(mediaType, ".tar"):
	default:
		return nil, false, false, fmt.Errorf("a %s layer, which fleet cannot read", mediaType)
	}
	ancestors := ancestorsOf(name)
	tr := tar.NewReader(stream)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, false, err
		}
		p := path.Clean(strings.TrimPrefix(h.Name, "/"))
		switch {
		case p == name:
			if h.Typeflag != tar.TypeReg {
				return nil, false, false, fmt.Errorf("/%s is not a regular file", name)
			}
			if h.Size > limit {
				return nil, false, false, fmt.Errorf("/%s is larger than %d bytes", name, limit)
			}
			if data, err = io.ReadAll(tr); err != nil {
				return nil, false, false, err
			}
			found = true
		case p == whiteout(name):
			removed = true
		default:
			for _, a := range ancestors {
				if p == whiteout(a) || p == path.Join(a, ".wh..wh..opq") || (p == a && h.Typeflag != tar.TypeDir) {
					removed = true
				}
			}
		}
	}
	// The rest of the blob (the archive's padding, the gzip trailer) is
	// part of what the digest covers.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return nil, false, false, err
	}
	if _, err := io.Copy(io.Discard, blob); err != nil {
		return nil, false, false, err
	}
	if err := verify(); err != nil {
		return nil, false, false, err
	}
	return data, found, removed, nil
}

// ancestorsOf are the directories above name ("etc/ssh/x": "etc",
// "etc/ssh").
func ancestorsOf(name string) []string {
	var out []string
	for d := path.Dir(name); d != "."; d = path.Dir(d) {
		out = append(out, d)
	}
	return out
}

// whiteout is the entry by which a layer removes p from the layers below.
func whiteout(p string) string {
	return path.Join(path.Dir(p), ".wh."+path.Base(p))
}
