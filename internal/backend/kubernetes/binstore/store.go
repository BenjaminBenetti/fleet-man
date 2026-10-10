// Package binstore finds the fleet binaries fleetd stages into Kubernetes
// pods: the pod build of fleet (`fleet-pod-linux-<arch>`, -tags nok8s, no
// client-go) of exactly the daemon's own version, and the static tmux bundle
// (`fleet-tmux-linux-<arch>.tar.gz`) that pods whose image has no tmux run
// their sessions on.
//
// A binary is looked up, per arch, in this order:
//
//  1. FLEET_POD_BINARY_<ARCH> (FLEET_POD_TMUX_<ARCH> for the bundle), a path
//     on the daemon host: the override. A wrong one is an error, never
//     skipped.
//  2. The cache, ~/.fleet/bin, which `make pod-binaries`, branch CI
//     artefacts or an earlier download fill.
//  3. The GitHub release of the daemon's own version, downloaded into the
//     cache and checked against the release's checksums.txt. Only a release
//     build has a release: a dev build finds its binaries in 1 or 2.
//
// A pod binary is served only when its build information says it is the pod
// build, for linux and the pod's arch, and the same build of fleet's source
// as the daemon (Identity.Equal). The daemon's own executable is never
// served: on a Linux host it is the full build, with client-go.
//
// The cached files may be rewritten in place (make writes them directly), so
// a file's sha256 is taken when it is looked up, and Artifact.Open checks it
// again as the file is read: a file that changed in between fails the read.
package binstore

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// Environment overrides: the prefix plus the arch in capitals
// (FLEET_POD_BINARY_AMD64).
const (
	EnvBinaryPrefix = "FLEET_POD_BINARY_"
	EnvTmuxPrefix   = "FLEET_POD_TMUX_"
)

// Release assets.
const (
	// DefaultReleaseBase is where a release's assets download from:
	// <base><version>/<asset>.
	DefaultReleaseBase = "https://github.com/BenjaminBenetti/fleet-man/releases/download/"
	// ChecksumsAsset is the release's `sha256sum fleet-*` listing.
	ChecksumsAsset = "checksums.txt"
	// maxDownload caps one downloaded asset.
	maxDownload = 512 << 20
	// downloadTimeout bounds one download.
	downloadTimeout = 10 * time.Minute
	// keepStaged is how many copies the staging cache keeps.
	keepStaged = 6
)

// BinaryAsset is the pod binary's name for arch (a GOARCH).
func BinaryAsset(arch string) string { return "fleet-pod-linux-" + arch }

// TmuxAsset is the tmux bundle's name for arch.
func TmuxAsset(arch string) string { return "fleet-tmux-linux-" + arch + ".tar.gz" }

// Kind is what an artifact is.
type Kind string

const (
	KindBinary Kind = "fleet-pod"
	KindTmux   Kind = "tmux"
)

// Source is where an artifact was found.
type Source string

const (
	SourceEnv     Source = "env"
	SourceCache   Source = "cache"
	SourceRelease Source = "release"
)

// Artifact is one file to stage.
type Artifact struct {
	Kind Kind
	Arch string
	Path string
	Size int64
	// SHA256 is the file's hex sha256 when it was looked up.
	SHA256 string
	Source Source
	// Info is a pod binary's build information (zero for the tmux bundle).
	Info Info
}

// MissingError is an artifact the store could not find for an arch, with
// why each place it looked fell through.
type MissingError struct {
	Kind  Kind
	Arch  string
	Tried []string
	Hint  string
}

func (e *MissingError) Error() string {
	msg := fmt.Sprintf("no %s binary for linux/%s", e.Kind, e.Arch)
	if e.Kind == KindTmux {
		msg = "no tmux bundle for linux/" + e.Arch
	}
	if len(e.Tried) > 0 {
		msg += " (" + strings.Join(e.Tried, "; ") + ")"
	}
	if e.Hint != "" {
		msg += ": " + e.Hint
	}
	return msg
}

// ErrOwnExecutable is a candidate that is the daemon's own executable.
var ErrOwnExecutable = errors.New("it is fleetd's own executable, never staged into a pod")

// Store finds pod artifacts. Its zero value is not usable: use New or
// Default. It is safe for concurrent use.
type Store struct {
	// Dir is the cache (~/.fleet/bin).
	Dir string
	// Self is the daemon's identity, which a pod binary must equal.
	Self Identity
	// SelfExe is the daemon's own executable, never served ("" for none).
	SelfExe string
	// Getenv reads the overrides (os.Getenv).
	Getenv func(string) string
	// Client downloads release assets; ReleaseBase is where from ("" turns
	// downloads off).
	Client      *http.Client
	ReleaseBase string

	mu        sync.Mutex
	hashes    map[string]fileHash
	checksums map[string]map[string]string
	selfSHA   string
	fetching  map[string]*fetch
	copying   map[string]*fetch
}

type fileHash struct {
	size  int64
	mtime time.Time
	sha   string
}

type fetch struct {
	done chan struct{}
	path string
	err  error
}

// New is a store over the cache dir for the running daemon.
func New(dir string) *Store {
	exe, _ := os.Executable()
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return &Store{
		Dir:         dir,
		Self:        Self(),
		SelfExe:     exe,
		Getenv:      os.Getenv,
		Client:      &http.Client{Timeout: downloadTimeout},
		ReleaseBase: DefaultReleaseBase,
	}
}

var (
	defaultOnce  sync.Once
	defaultStore *Store
)

// Default is the daemon's store over ~/.fleet/bin.
func Default() *Store {
	defaultOnce.Do(func() { defaultStore = New(filepath.Join(fleetpaths.Dir(), "bin")) })
	return defaultStore
}

// Binary is the pod binary for arch.
func (s *Store) Binary(ctx context.Context, arch string) (*Artifact, error) {
	return s.find(ctx, KindBinary, arch)
}

// Tmux is the tmux bundle for arch.
func (s *Store) Tmux(ctx context.Context, arch string) (*Artifact, error) {
	return s.find(ctx, KindTmux, arch)
}

func (s *Store) find(ctx context.Context, kind Kind, arch string) (*Artifact, error) {
	if arch == "" || strings.ContainsAny(arch, "/\\ ") {
		return nil, fmt.Errorf("bad arch %q", arch)
	}
	envName, asset := EnvBinaryPrefix+strings.ToUpper(arch), BinaryAsset(arch)
	if kind == KindTmux {
		envName, asset = EnvTmuxPrefix+strings.ToUpper(arch), TmuxAsset(arch)
	}
	if p := s.getenv(envName); p != "" {
		a, err := s.check(kind, arch, p, SourceEnv)
		if err != nil {
			return nil, fmt.Errorf("%s=%s: %w", envName, p, err)
		}
		return a, nil
	}
	var tried []string
	cached := filepath.Join(s.Dir, asset)
	switch a, err := s.check(kind, arch, cached, SourceCache); {
	case err == nil:
		return a, nil
	case errors.Is(err, os.ErrNotExist):
		tried = append(tried, cached+" does not exist")
	default:
		tried = append(tried, err.Error())
	}
	if s.Self.Version == "" || s.ReleaseBase == "" {
		hint := "build them with `make pod-binaries`, or point " + envName + " at one"
		if s.Self.Version == "" {
			tried = append(tried, "a dev build has no release to download")
		}
		return nil, &MissingError{Kind: kind, Arch: arch, Tried: tried, Hint: hint}
	}
	path, err := s.download(ctx, asset)
	if err != nil {
		tried = append(tried, err.Error())
		return nil, &MissingError{Kind: kind, Arch: arch, Tried: tried, Hint: "check the daemon host can reach github.com, or point " + envName + " at one"}
	}
	a, err := s.check(kind, arch, path, SourceRelease)
	if err != nil {
		_ = os.Remove(path)
		tried = append(tried, "the "+s.Self.Version+" release's: "+err.Error())
		return nil, &MissingError{Kind: kind, Arch: arch, Tried: tried}
	}
	return a, nil
}

func (s *Store) getenv(name string) string {
	if s.Getenv == nil {
		return os.Getenv(name)
	}
	return s.Getenv(name)
}

// check vets the file at path as kind for arch.
func (s *Store) check(kind Kind, arch, path string, src Source) (*Artifact, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	a := &Artifact{Kind: kind, Arch: arch, Path: path, Source: src}
	if kind == KindBinary {
		if s.isSelf(fi) {
			return nil, fmt.Errorf("%s: %w", path, ErrOwnExecutable)
		}
		info, err := Inspect(path)
		if err != nil {
			return nil, err
		}
		switch {
		case info.Flavor != buildflavor.Pod:
			return nil, fmt.Errorf("%s is the %s build of fleet, not the pod build (-tags %s)", path, info.Flavor, buildflavor.PodTag)
		case info.GOOS != "linux" || info.GOARCH != arch:
			return nil, fmt.Errorf("%s is built for %s/%s, not linux/%s", path, info.GOOS, info.GOARCH, arch)
		case !info.Equal(s.Self):
			return nil, fmt.Errorf("%s is %s, not the daemon's %s", path, info.Identity, s.Self)
		}
		a.Info = info
	} else if err := checkTmuxBundle(path, arch); err != nil {
		return nil, err
	}
	sha, size, err := s.hash(path, fi)
	if err != nil {
		return nil, err
	}
	if kind == KindBinary && s.sameAsSelf(sha) {
		return nil, fmt.Errorf("%s: %w", path, ErrOwnExecutable)
	}
	a.SHA256, a.Size = sha, size
	return a, nil
}

// isSelf reports whether fi is the daemon's own executable.
func (s *Store) isSelf(fi os.FileInfo) bool {
	if s.SelfExe == "" {
		return false
	}
	self, err := os.Stat(s.SelfExe)
	return err == nil && os.SameFile(fi, self)
}

// sameAsSelf reports whether sha is the daemon's own executable's content
// (a copy of it), hashing the executable once.
func (s *Store) sameAsSelf(sha string) bool {
	if s.SelfExe == "" {
		return false
	}
	s.mu.Lock()
	self := s.selfSHA
	s.mu.Unlock()
	if self == "" {
		var err error
		if self, _, err = hashFile(s.SelfExe); err != nil {
			return false
		}
		s.mu.Lock()
		s.selfSHA = self
		s.mu.Unlock()
	}
	return self == sha
}

// hash is path's sha256 and size, cached while its size and mtime stay.
func (s *Store) hash(path string, fi os.FileInfo) (string, int64, error) {
	s.mu.Lock()
	h, ok := s.hashes[path]
	s.mu.Unlock()
	if ok && h.size == fi.Size() && h.mtime.Equal(fi.ModTime()) {
		return h.sha, h.size, nil
	}
	sha, size, err := hashFile(path)
	if err != nil {
		return "", 0, err
	}
	if size == fi.Size() {
		s.mu.Lock()
		if s.hashes == nil {
			s.hashes = map[string]fileHash{}
		}
		s.hashes[path] = fileHash{size: size, mtime: fi.ModTime(), sha: sha}
		s.mu.Unlock()
	}
	return sha, size, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Open reads the artifact. The read fails at its end when the file is no
// longer the one looked up (another size or sha256).
func (a *Artifact) Open() (io.ReadCloser, error) {
	f, err := os.Open(a.Path)
	if err != nil {
		return nil, err
	}
	return &checkedReader{f: f, h: sha256.New(), want: a.SHA256, size: a.Size, name: a.Path}, nil
}

type checkedReader struct {
	f    *os.File
	h    hash.Hash
	n    int64
	want string
	size int64
	name string
}

func (r *checkedReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if n > 0 {
		r.n += int64(n)
		_, _ = r.h.Write(p[:n])
		if r.n > r.size {
			return n, fmt.Errorf("%s grew while it was read", r.name)
		}
	}
	if errors.Is(err, io.EOF) {
		if got := hex.EncodeToString(r.h.Sum(nil)); r.n != r.size || got != r.want {
			return n, fmt.Errorf("%s changed while it was read (%d bytes, sha256 %s; want %d, %s)", r.name, r.n, got, r.size, r.want)
		}
	}
	return n, err
}

func (r *checkedReader) Close() error { return r.f.Close() }

// Staged is a private copy of the artifact to stream into a pod, made once
// and kept in the cache (Dir/.staged/<sha256>, or <sha256>.gz compressed
// for a pod that has gzip): it is written from Open, so it is exactly the
// sha256 the artifact was looked up with, and it does not change while it is
// streamed, whatever happens to the file it came from. It returns the copy's
// path and size.
func (s *Store) Staged(ctx context.Context, a *Artifact, compressed bool) (string, int64, error) {
	dir := filepath.Join(s.Dir, ".staged")
	path := filepath.Join(dir, a.SHA256)
	if compressed {
		path += ".gz"
	}
	if fi, err := os.Stat(path); err == nil {
		return path, fi.Size(), nil
	}
	f, leader := s.join(&s.copying, path)
	if leader {
		f.err = s.copyOut(a, dir, path, compressed)
		s.finish(&s.copying, path, f)
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	if f.err != nil {
		return "", 0, f.err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return path, fi.Size(), nil
}

func (s *Store) copyOut(a *Artifact, dir, path string, compressed bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	in, err := a.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	var w io.Writer = tmp
	var zw *gzip.Writer
	if compressed {
		zw = gzip.NewWriter(tmp)
		w = zw
	}
	if _, err := io.Copy(w, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s: %w", a.Path, err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	pruneOldest(dir, "", keepStaged)
	return nil
}

// pruneOldest keeps the keep newest files with suffix in dir (dot files,
// which are copies in progress, aside).
func pruneOldest(dir, suffix string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		name  string
		mtime time.Time
	}
	var files []file
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), suffix) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, file{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	for _, f := range files[min(keep, len(files)):] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

// join joins the in-flight operation on key in *m, or starts one (leader).
func (s *Store) join(m *map[string]*fetch, key string) (*fetch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := (*m)[key]; ok {
		return f, false
	}
	if *m == nil {
		*m = map[string]*fetch{}
	}
	f := &fetch{done: make(chan struct{})}
	(*m)[key] = f
	return f, true
}

func (s *Store) finish(m *map[string]*fetch, key string, f *fetch) {
	s.mu.Lock()
	delete(*m, key)
	s.mu.Unlock()
	close(f.done)
}

// download fetches the daemon's release's asset into the cache, checked
// against the release's checksums, and returns its path.
func (s *Store) download(ctx context.Context, asset string) (string, error) {
	f, leader := s.join(&s.fetching, asset)
	if leader {
		f.path, f.err = s.fetchAsset(ctx, asset)
		s.finish(&s.fetching, asset, f)
	}
	select {
	case <-f.done:
		return f.path, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Store) fetchAsset(ctx context.Context, asset string) (string, error) {
	sums, err := s.releaseChecksums(ctx)
	if err != nil {
		return "", err
	}
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("the %s release has no %s", s.Self.Version, asset)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Dir, "."+asset+".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if err := s.get(ctx, asset, io.MultiWriter(tmp, h)); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = tmp.Close()
		return "", fmt.Errorf("the downloaded %s has sha256 %s, the release lists %s", asset, got, want)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(s.Dir, asset)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// releaseChecksums is the daemon's release's checksums.txt, fetched once.
func (s *Store) releaseChecksums(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	sums, ok := s.checksums[s.Self.Version]
	s.mu.Unlock()
	if ok {
		return sums, nil
	}
	var b strings.Builder
	if err := s.get(ctx, ChecksumsAsset, &b); err != nil {
		return nil, err
	}
	sums = map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && len(fields[0]) == 64 {
			sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}
	s.mu.Lock()
	if s.checksums == nil {
		s.checksums = map[string]map[string]string{}
	}
	s.checksums[s.Self.Version] = sums
	s.mu.Unlock()
	return sums, nil
}

// get downloads one asset of the daemon's release into w.
func (s *Store) get(ctx context.Context, asset string, w io.Writer) error {
	url := s.ReleaseBase + s.Self.Version + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxDownload {
		return fmt.Errorf("download %s: over %d bytes", url, maxDownload)
	}
	return nil
}
