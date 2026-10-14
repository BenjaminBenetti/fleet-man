package imageserve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// save.go turns `docker save` output into served content. Three layouts
// arrive:
//
//   - docker's containerd image store (the default on fresh Docker 29
//     installs): an OCI layout whose index.json names the image's own index,
//     which lists every platform (only the ones the host has are in the
//     archive) and buildkit's attestation manifests; layers are compressed
//     (gzip or zstd) and are served byte for byte, so the node-platform
//     manifest keeps its digest;
//   - docker's classic overlay2 store (Docker 25+): an OCI layout whose
//     index.json names one manifest with uncompressed layers;
//   - older docker: manifest.json only, with <id>/layer.tar layers.
//
// Uncompressed layers are gzip-compressed once, keyed by diffID (the layer's
// own digest), and the resulting manifest is written once.

// maxIndexSize bounds the JSON documents read into memory (index.json,
// manifest.json, manifests, configs).
const maxIndexSize = 4 << 20

// docker resolves the daemon host's docker.
func (s *Store) docker() (string, error) {
	p := s.Docker
	if p == "" {
		p = "docker"
	}
	if !strings.Contains(p, "/") {
		found, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("docker is not installed on the daemon host: %w", err)
		}
		p = found
	}
	return filepath.Abs(p)
}

// saveAndImport exports imageID with `docker save` and imports it.
func (s *Store) saveAndImport(ctx context.Context, imageID string, p platform) (Prepared, error) {
	docker, err := s.docker()
	if err != nil {
		return Prepared{}, err
	}
	// Ask first: a save of a missing image fails differently on each image
	// store (the classic one names a file in its image database).
	if out, err := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{.Id}}", "--", imageID).CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return Prepared{}, ctx.Err()
		}
		if isNoSuchImage(string(out)) {
			return Prepared{}, &ImageGoneError{ImageID: imageID}
		}
		return Prepared{}, fmt.Errorf("docker image inspect %s: %v: %s", short(imageID), err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(backend.JobLogFrom(ctx), "exporting image %s for gateway delivery (docker save)\n", short(imageID))
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "save", imageID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Prepared{}, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Prepared{}, fmt.Errorf("docker save: %w", err)
	}
	prep, ierr := s.Import(ctx, stdout, imageID, p.String())
	if ierr != nil {
		// Stop docker; what it said, if it failed by itself, is the cause.
		cancel()
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	if err := parent.Err(); err != nil {
		return Prepared{}, err
	}
	if msg := strings.TrimSpace(stderr.String()); werr != nil && (ierr == nil || msg != "") {
		if isNoSuchImage(msg) {
			return Prepared{}, &ImageGoneError{ImageID: imageID}
		}
		return Prepared{}, fmt.Errorf("docker save %s: %v: %s", short(imageID), werr, msg)
	}
	if ierr != nil {
		return Prepared{}, ierr
	}
	return prep, nil
}

// isNoSuchImage reports whether docker's message says the image is not
// there.
func isNoSuchImage(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "no such image") || strings.Contains(low, "reference does not exist")
}

// ImageGoneError: the daemon host no longer has an image that was never
// prepared (or whose cached copy is gone).
type ImageGoneError struct{ ImageID string }

func (e *ImageGoneError) Error() string {
	return fmt.Sprintf("image %s is no longer on the daemon host", short(e.ImageID))
}

// tailBuffer keeps the last 4 KiB written to it (a command's stderr).
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// Import reads a `docker save` archive of imageID from r and makes its
// manifest for plat servable, as Prepare does after running docker save.
func (s *Store) Import(ctx context.Context, r io.Reader, imageID, plat string) (Prepared, error) {
	p, err := parsePlatform(plat)
	if err != nil {
		return Prepared{}, err
	}
	s.gc.RLock()
	defer s.gc.RUnlock()
	dir, err := s.newTempDir()
	if err != nil {
		return Prepared{}, err
	}
	defer os.RemoveAll(dir)
	im := &saveImport{s: s, dir: dir, log: backend.JobLogFrom(ctx), files: map[string]*stagedFile{}, byDigest: map[string]*stagedFile{}}
	if err := im.read(ctx, r); err != nil {
		return Prepared{}, err
	}
	var c candidate
	switch {
	case im.indexJSON != nil:
		c, err = im.fromLayout(p)
	case im.manifestJSON != nil:
		c, err = im.fromLegacy(p)
	default:
		err = errors.New("docker save output has neither index.json nor manifest.json")
	}
	if err != nil {
		return Prepared{}, fmt.Errorf("image %s: %w", short(imageID), err)
	}
	d, rec, diffIDs, err := im.build(ctx, c)
	if err != nil {
		return Prepared{}, fmt.Errorf("image %s: %w", short(imageID), err)
	}
	rec.ImageID = imageID
	rec.Platform = p.String()
	prep, err := s.recordImport(imageID, p, d, rec, diffIDs)
	if err != nil {
		return Prepared{}, err
	}
	fmt.Fprintf(im.log, "image %s ready for gateway delivery: %s manifest %s (%d layers, %s)\n", short(imageID), p, short(d), len(rec.Blobs)-1, humanBytes(prep.Bytes))
	return prep, nil
}

// stagedFile is one regular file of the archive.
type stagedFile struct {
	// path is where it was staged; "" when the cache already had it (or the
	// gzip blob made from it), so it was not written.
	path   string
	digest string
	size   int64
	// head is its first bytes, for sniffing compression.
	head []byte
	// cached: the cache already holds this blob.
	cached bool
	// gzipped is the cached gzip blob made from this (uncompressed) layer,
	// when the cache had one.
	gzipped     string
	gzippedSize int64
	// link is a symlink's target path (older docker links duplicate
	// layers).
	link string
}

type saveImport struct {
	s   *Store
	dir string
	log io.Writer
	// files are the archive's files by cleaned path; byDigest the
	// content-addressed ones by digest.
	files    map[string]*stagedFile
	byDigest map[string]*stagedFile
	// indexJSON and manifestJSON are the archive's index.json and
	// manifest.json.
	indexJSON, manifestJSON []byte
}

// ctxReader stops a copy once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// read stages the archive: content-addressed blobs the cache lacks are
// written (and verified) under the import directory, ones it has are
// skipped, and the layout documents are kept in memory.
func (im *saveImport) read(ctx context.Context, r io.Reader) error {
	tr := tar.NewReader(ctxReader{ctx, r})
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("reading docker save output: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("docker save output has an unsafe path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeSymlink:
			im.files[name] = &stagedFile{link: path.Clean(path.Join(path.Dir(name), h.Linkname))}
			continue
		case tar.TypeReg:
		default:
			continue
		}
		switch {
		case name == "index.json" || name == "manifest.json":
			b, err := io.ReadAll(io.LimitReader(tr, maxIndexSize+1))
			if err != nil {
				return fmt.Errorf("reading docker save output: %w", err)
			}
			if len(b) > maxIndexSize {
				return fmt.Errorf("docker save output: %s is too large", name)
			}
			if name == "index.json" {
				im.indexJSON = b
			} else {
				im.manifestJSON = b
			}
			continue
		case name == "oci-layout" || name == "repositories":
			continue
		}
		want := ""
		if hexPart, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && validDigest("sha256:"+hexPart) {
			want = "sha256:" + hexPart
			if im.s.hasBlob(want) {
				f := &stagedFile{digest: want, size: h.Size, cached: true}
				im.files[name], im.byDigest[want] = f, f
				continue
			}
			if gz, size, ok := im.s.compressedFor(want); ok {
				f := &stagedFile{digest: want, size: h.Size, gzipped: gz, gzippedSize: size}
				im.files[name], im.byDigest[want] = f, f
				continue
			}
		}
		f, err := im.stage(tr, want)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		im.files[name] = f
		im.byDigest[f.digest] = f
	}
}

// stage writes one file under the import directory, hashing it; want, when
// set, is the digest it must have.
func (im *saveImport) stage(r io.Reader, want string) (*stagedFile, error) {
	out, err := os.CreateTemp(im.dir, "blob-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	head := &headWriter{}
	n, err := io.Copy(io.MultiWriter(out, h, head), r)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("staging docker save output: %w", err)
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if want != "" && d != want {
		return nil, fmt.Errorf("docker save output: blob %s does not match its digest", short(want))
	}
	return &stagedFile{path: out.Name(), digest: d, size: n, head: head.b}, nil
}

// headWriter keeps the first bytes written to it.
type headWriter struct{ b []byte }

func (w *headWriter) Write(p []byte) (int, error) {
	if len(w.b) < 8 {
		w.b = append(w.b, p[:min(len(p), 8-len(w.b))]...)
	}
	return len(p), nil
}

// resolve follows a file's symlinks.
func (im *saveImport) resolve(name string) (*stagedFile, bool) {
	for range 8 {
		f, ok := im.files[path.Clean(name)]
		if !ok {
			return nil, false
		}
		if f.link == "" {
			return f, true
		}
		name = f.link
	}
	return nil, false
}

// document returns a small file's content (a manifest, index or config),
// staged or cached, by digest.
func (im *saveImport) document(digest string) ([]byte, bool) {
	f, ok := im.byDigest[digest]
	if !ok {
		return nil, false
	}
	return im.readFile(f)
}

func (im *saveImport) readFile(f *stagedFile) ([]byte, bool) {
	p := f.path
	if f.cached {
		p = im.s.blobPath(f.digest)
	}
	if p == "" || f.size > maxIndexSize {
		return nil, false
	}
	b, err := os.ReadFile(p)
	return b, err == nil
}

// candidate is a runnable image manifest the archive holds.
type candidate struct {
	// digest of the manifest in the archive ("" for an older docker's
	// manifest.json, which has none).
	digest string
	// raw is the manifest as the archive holds it (nil for manifest.json).
	raw []byte
	m   manifest
	// os, arch, variant from the image config (or the descriptor).
	os, arch, variant string
}

func (c candidate) platformString() string {
	return platform{os: c.os, arch: c.arch, variant: c.variant}.String()
}

// fromLayout picks the manifest for p from an OCI layout's index.json.
func (im *saveImport) fromLayout(p platform) (candidate, error) {
	var top index
	if err := json.Unmarshal(im.indexJSON, &top); err != nil {
		return candidate{}, fmt.Errorf("docker save output: unreadable index.json: %w", err)
	}
	var cands []candidate
	var missing []string
	var walk func(descs []descriptor, depth int)
	walk = func(descs []descriptor, depth int) {
		for _, d := range descs {
			if !validDigest(d.Digest) || d.Annotations[attestationAnnotation] == "attestation-manifest" {
				continue
			}
			if d.Platform != nil && (d.Platform.OS == "unknown" || d.Platform.Architecture == "unknown") {
				continue
			}
			b, ok := im.document(d.Digest)
			if !ok {
				if d.Platform != nil {
					missing = append(missing, platform{os: d.Platform.OS, arch: d.Platform.Architecture, variant: d.Platform.Variant}.String())
				}
				continue
			}
			mt := d.MediaType
			if mt == "" {
				var probe struct {
					MediaType string          `json:"mediaType"`
					Manifests json.RawMessage `json:"manifests"`
				}
				_ = json.Unmarshal(b, &probe)
				mt = probe.MediaType
				if mt == "" && probe.Manifests != nil {
					mt = mediaOCIIndex
				}
			}
			if isIndexType(mt) {
				var ix index
				if depth < 4 && json.Unmarshal(b, &ix) == nil {
					walk(ix.Manifests, depth+1)
				}
				continue
			}
			var m manifest
			if json.Unmarshal(b, &m) != nil || !validDigest(m.Config.Digest) {
				continue
			}
			c := candidate{digest: d.Digest, raw: b, m: m}
			if cb, ok := im.document(m.Config.Digest); ok {
				var cfg imageConfig
				if json.Unmarshal(cb, &cfg) == nil {
					c.os, c.arch, c.variant = cfg.OS, cfg.Architecture, cfg.Variant
				}
			}
			if c.os == "" && d.Platform != nil {
				c.os, c.arch, c.variant = d.Platform.OS, d.Platform.Architecture, d.Platform.Variant
			}
			if c.os == "unknown" || c.arch == "unknown" {
				continue
			}
			cands = append(cands, c)
		}
	}
	walk(top.Manifests, 0)
	return pick(cands, missing, p)
}

// pick returns the candidate for p, or an error naming what the host has.
func pick(cands []candidate, missing []string, p platform) (candidate, error) {
	var have []string
	for _, c := range cands {
		if p.matches(c.os, c.arch, c.variant) {
			return c, nil
		}
		have = append(have, c.platformString())
	}
	switch {
	case len(have) > 0:
		return candidate{}, fmt.Errorf("the daemon host has it for %s, not for the nodes' %s", strings.Join(have, ", "), p)
	case len(missing) > 0:
		return candidate{}, fmt.Errorf("the daemon host has none of its platforms' content (it lists %s)", strings.Join(missing, ", "))
	}
	return candidate{}, errors.New("docker save output names no image manifest")
}

// fromLegacy builds the candidate of an older docker's manifest.json.
func (im *saveImport) fromLegacy(p platform) (candidate, error) {
	var entries []struct {
		Config       string                `json:"Config"`
		Layers       []string              `json:"Layers"`
		LayerSources map[string]descriptor `json:"LayerSources"`
	}
	if err := json.Unmarshal(im.manifestJSON, &entries); err != nil || len(entries) == 0 {
		return candidate{}, fmt.Errorf("docker save output: unreadable manifest.json: %v", err)
	}
	e := entries[0]
	cf, ok := im.resolve(e.Config)
	if !ok {
		return candidate{}, fmt.Errorf("docker save output lacks the image config %s", e.Config)
	}
	cb, ok := im.readFile(cf)
	if !ok {
		return candidate{}, errors.New("docker save output: unreadable image config")
	}
	var cfg imageConfig
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return candidate{}, fmt.Errorf("docker save output: unreadable image config: %w", err)
	}
	c := candidate{os: cfg.OS, arch: cfg.Architecture, variant: cfg.Variant}
	c.m = manifest{SchemaVersion: 2, MediaType: mediaOCIManifest, Config: descriptor{MediaType: mediaOCIConfig, Digest: cf.digest, Size: cf.size}}
	for _, l := range e.Layers {
		f, ok := im.resolve(l)
		if !ok {
			return candidate{}, fmt.Errorf("docker save output lacks the layer %s", l)
		}
		mt := ""
		if src, ok := e.LayerSources[f.digest]; ok {
			mt = src.MediaType
		}
		c.m.Layers = append(c.m.Layers, descriptor{MediaType: mt, Digest: f.digest, Size: f.size})
	}
	return pick([]candidate{c}, nil, p)
}

// build puts the candidate's config and layers in the cache (compressing
// uncompressed layers) and returns its manifest: the archive's own, byte for
// byte, when no layer needed compressing, else a new one.
func (im *saveImport) build(ctx context.Context, c candidate) (string, *manifestRecord, map[string]string, error) {
	rec := &manifestRecord{}
	cfg, ok := im.byDigest[c.m.Config.Digest]
	if !ok {
		return "", nil, nil, fmt.Errorf("docker save output lacks the image config %s", short(c.m.Config.Digest))
	}
	if c.raw != nil && c.m.Config.Size != cfg.size {
		return "", nil, nil, fmt.Errorf("docker save output: the image config is %d bytes, its manifest says %d", cfg.size, c.m.Config.Size)
	}
	if err := im.keep(cfg); err != nil {
		return "", nil, nil, err
	}
	rec.Blobs = append(rec.Blobs, blobRecord{Digest: cfg.digest, Size: cfg.size})

	diffIDs := map[string]string{}
	changed := c.raw == nil
	layers := make([]descriptor, len(c.m.Layers))
	for i, l := range c.m.Layers {
		f, ok := im.byDigest[l.Digest]
		if !ok {
			if len(l.URLs) > 0 {
				return "", nil, nil, fmt.Errorf("layer %s is a foreign layer the daemon host does not hold", short(l.Digest))
			}
			return "", nil, nil, fmt.Errorf("docker save output lacks the layer %s", short(l.Digest))
		}
		compressed := isCompressedLayer(l.MediaType)
		if l.MediaType == "" {
			compressed = bytes.HasPrefix(f.head, gzipMagic) || bytes.HasPrefix(f.head, zstdMagic)
			if compressed {
				l.MediaType = mediaOCILayerGzip
				if bytes.HasPrefix(f.head, zstdMagic) {
					l.MediaType = mediaOCILayer + "+zstd"
				}
			}
		}
		if compressed {
			if c.raw != nil && l.Size != f.size {
				return "", nil, nil, fmt.Errorf("docker save output: layer %s is %d bytes, its manifest says %d", short(l.Digest), f.size, l.Size)
			}
			if err := im.keep(f); err != nil {
				return "", nil, nil, err
			}
			layers[i] = l
			layers[i].Size = f.size
			rec.Blobs = append(rec.Blobs, blobRecord{Digest: f.digest, Size: f.size})
			continue
		}
		gz, size := f.gzipped, f.gzippedSize
		if gz == "" {
			var err error
			if gz, size, err = im.compress(ctx, f); err != nil {
				return "", nil, nil, err
			}
		}
		diffIDs[f.digest] = gz
		layers[i] = descriptor{MediaType: gzipLayerType(l.MediaType), Digest: gz, Size: size, Annotations: l.Annotations}
		changed = true
		rec.Blobs = append(rec.Blobs, blobRecord{Digest: gz, Size: size})
	}

	var raw []byte
	mediaType := c.m.MediaType
	if !changed {
		raw = c.raw
		if mediaType == "" {
			mediaType = mediaOCIManifest
		}
		if f := im.byDigest[c.digest]; f != nil {
			if err := im.keep(f); err != nil {
				return "", nil, nil, err
			}
		}
	} else {
		if mediaType != mediaDockerV2 {
			mediaType = mediaOCIManifest
		}
		m := manifest{SchemaVersion: 2, MediaType: mediaType, Config: c.m.Config, Layers: layers, Annotations: c.m.Annotations}
		var err error
		if raw, err = json.Marshal(m); err != nil {
			return "", nil, nil, err
		}
		if err := im.writeBlob(raw); err != nil {
			return "", nil, nil, err
		}
	}
	d := sha256Digest(raw)
	if !changed && c.digest != "" && d != c.digest {
		return "", nil, nil, fmt.Errorf("docker save output: manifest %s does not match its digest", short(c.digest))
	}
	rec.MediaType = mediaType
	rec.Size = int64(len(raw))
	return d, rec, diffIDs, nil
}

// keep moves a staged file into the cache (a cached one is there already).
func (im *saveImport) keep(f *stagedFile) error {
	if f.cached {
		return nil
	}
	if f.path == "" {
		return fmt.Errorf("blob %s was not staged", short(f.digest))
	}
	if err := im.s.install(f.path, f.digest); err != nil {
		return err
	}
	f.path, f.cached = "", true
	return nil
}

// writeBlob writes a small blob (a manifest fleet made) into the cache.
func (im *saveImport) writeBlob(b []byte) error {
	out, err := os.CreateTemp(im.dir, "manifest-*")
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return im.s.install(out.Name(), sha256Digest(b))
}

// compress gzip-compresses an uncompressed layer into the cache and returns
// the compressed blob's digest and size. Go's gzip output carries no time or
// name, so a layer compresses to the same digest every time.
func (im *saveImport) compress(ctx context.Context, f *stagedFile) (string, int64, error) {
	if f.path == "" {
		return "", 0, fmt.Errorf("layer %s was not staged", short(f.digest))
	}
	fmt.Fprintf(im.log, "compressing layer %s (%s)\n", short(f.digest), humanBytes(f.size))
	in, err := os.Open(f.path)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.CreateTemp(im.dir, "gzip-*")
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	var n countWriter
	gz, err := gzip.NewWriterLevel(io.MultiWriter(out, h, &n), gzip.DefaultCompression)
	if err != nil {
		_ = out.Close()
		return "", 0, err
	}
	_, err = io.Copy(gz, ctxReader{ctx, in})
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, fmt.Errorf("compressing layer %s: %w", short(f.digest), err)
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if err := im.s.install(out.Name(), d); err != nil {
		return "", 0, err
	}
	_ = os.Remove(f.path)
	// A layer listed twice (an older docker's duplicates) compresses once.
	f.path, f.gzipped, f.gzippedSize = "", d, int64(n)
	return d, int64(n), nil
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}

// humanBytes renders a size for the provisioning log.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
