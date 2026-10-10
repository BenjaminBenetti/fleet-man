package binstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
)

var (
	buildsMu sync.Mutex
	builds   = map[string]string{}
	buildDir string
)

// program builds a minimal Go program for goos/goarch, with -tags nok8s
// when pod is set and the version stamped like a release (none when ""),
// outside any VCS checkout so it records no revision. Builds are shared by
// the package's tests.
func program(t *testing.T, goos, goarch string, pod bool, version string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build test binaries with")
	}
	key := fmt.Sprintf("%s-%s-%v-%s", goos, goarch, pod, version)
	buildsMu.Lock()
	defer buildsMu.Unlock()
	if p, ok := builds[key]; ok {
		return p
	}
	if buildDir == "" {
		dir, err := os.MkdirTemp("", "binstore-test-")
		if err != nil {
			t.Fatal(err)
		}
		buildDir = dir
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/podbin\n\ngo 1.25\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(buildDir, key)
	args := []string{"build", "-o", out}
	if pod {
		args = append(args, "-tags", buildflavor.PodTag)
	}
	ld := "-s -w"
	if version != "" {
		ld += " -X " + VersionSymbol + "=" + version
	}
	args = append(args, "-ldflags", ld, ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = buildDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOFLAGS=")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build a test binary: %v\n%s", err, msg)
	}
	builds[key] = out
	return out
}

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func copyTo(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func shaOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func testStore(t *testing.T, self Identity, env map[string]string) *Store {
	t.Helper()
	return &Store{Dir: t.TempDir(), Self: self, Getenv: func(k string) string { return env[k] }}
}

func TestStampedVersion(t *testing.T) {
	for ldflags, want := range map[string]string{
		"":                                    "",
		"-s -w":                               "",
		"-s -w -X " + VersionSymbol + "=v1.2": "v1.2",
		"-X=" + VersionSymbol + "=v3":         "v3",
		"-X '" + VersionSymbol + "=v4'":       "v4",
		"-X other.Var=x -X " + VersionSymbol + "=v5 -X " + VersionSymbol + "=v6": "v6",
		"-X " + VersionSymbol + "=":    "",
		"-X " + VersionSymbol + "x=v7": "",
	} {
		if got := stampedVersion(ldflags); got != want {
			t.Errorf("stampedVersion(%q) = %q, want %q", ldflags, got, want)
		}
	}
}

func TestIdentityEqual(t *testing.T) {
	tests := []struct {
		a, b Identity
		want bool
	}{
		{Identity{Version: "v1"}, Identity{Version: "v1", Revision: "x"}, true},
		{Identity{Version: "v1"}, Identity{Version: "v2"}, false},
		{Identity{Version: "v1"}, Identity{Revision: "abc"}, false},
		{Identity{Revision: "abc"}, Identity{Revision: "abc"}, true},
		{Identity{Revision: "abc"}, Identity{Revision: "abd"}, false},
		{Identity{Revision: "abc", Modified: true}, Identity{Revision: "abc"}, false},
		{Identity{}, Identity{}, true},
		{Identity{}, Identity{Revision: "abc"}, false},
	}
	for _, tt := range tests {
		if got := tt.a.Equal(tt.b); got != tt.want || tt.b.Equal(tt.a) != tt.want {
			t.Errorf("%+v.Equal(%+v) = %v, want %v (both ways)", tt.a, tt.b, got, tt.want)
		}
	}
	if s := (Identity{Revision: "0123456789abcdef", Modified: true}).String(); s != "dev build 0123456789ab+changes" {
		t.Errorf("String = %q", s)
	}
}

func TestVCSSettings(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"}}}
	if rev, mod := vcs(bi); rev != "abc" || !mod {
		t.Fatalf("vcs = %q %v", rev, mod)
	}
}

func TestInspectReadsAnotherArchsBuild(t *testing.T) {
	bin := program(t, "linux", "arm64", true, "v9.9.9")
	info, err := Inspect(bin)
	if err != nil {
		t.Fatal(err)
	}
	if info.Flavor != buildflavor.Pod || info.GOOS != "linux" || info.GOARCH != "arm64" || info.Version != "v9.9.9" || info.Revision != "" {
		t.Fatalf("info %+v", info)
	}
	if _, err := Inspect(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("inspected a missing file")
	}
}

func TestBinaryLookupOrder(t *testing.T) {
	self := Identity{Version: "v9.9.9"}
	good := program(t, "linux", "amd64", true, "v9.9.9")
	other := program(t, "linux", "amd64", true, "v9.9.8")
	env := map[string]string{}
	s := testStore(t, self, env)
	ctx := context.Background()

	var missing *MissingError
	if _, err := s.Binary(ctx, "amd64"); !errors.As(err, &missing) || !strings.Contains(err.Error(), "make pod-binaries") {
		t.Fatalf("nothing anywhere: %v", err)
	}

	cached := filepath.Join(s.Dir, BinaryAsset("amd64"))
	copyTo(t, good, cached)
	a, err := s.Binary(ctx, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != SourceCache || a.Path != cached || a.SHA256 != shaOf(t, good) || a.Kind != KindBinary || a.Info.Version != "v9.9.9" {
		t.Fatalf("cached artifact %+v", a)
	}

	// The override wins, and a wrong override is an error, never skipped.
	override := filepath.Join(t.TempDir(), "fleet")
	copyTo(t, good, override)
	env[EnvBinaryPrefix+"AMD64"] = override
	if a, err := s.Binary(ctx, "amd64"); err != nil || a.Source != SourceEnv || a.Path != override {
		t.Fatalf("override: %+v %v", a, err)
	}
	env[EnvBinaryPrefix+"AMD64"] = other
	if _, err := s.Binary(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "not the daemon's v9.9.9") {
		t.Fatalf("an override of another version: %v", err)
	}
	delete(env, EnvBinaryPrefix+"AMD64")

	// Every reason a cached file is refused.
	for name, bin := range map[string]string{
		"full build":   program(t, "linux", "amd64", false, "v9.9.9"),
		"another arch": program(t, "linux", "arm64", true, "v9.9.9"),
		"darwin":       program(t, "darwin", "amd64", true, "v9.9.9"),
		"version":      other,
	} {
		copyTo(t, bin, cached)
		_, err := s.Binary(ctx, "amd64")
		if !errors.As(err, &missing) {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: %v", name, err)
	}
	if _, err := s.Binary(ctx, "../x"); err == nil {
		t.Fatal("a path as an arch")
	}
}

func TestDevBuildsCompareRevisions(t *testing.T) {
	// The test builds record no revision: they equal a daemon that has none
	// either, and differ from one that has one.
	bin := program(t, "linux", "amd64", true, "")
	s := testStore(t, Identity{}, nil)
	copyTo(t, bin, filepath.Join(s.Dir, BinaryAsset("amd64")))
	if _, err := s.Binary(context.Background(), "amd64"); err != nil {
		t.Fatalf("revisionless dev builds: %v", err)
	}
	s.Self = Identity{Revision: "0123456789abcdef"}
	if _, err := s.Binary(context.Background(), "amd64"); err == nil || !strings.Contains(err.Error(), "a dev build has no release") {
		t.Fatalf("dev daemon at a commit: %v", err)
	}
}

func TestNeverServesTheDaemonsOwnExecutable(t *testing.T) {
	bin := program(t, "linux", "amd64", true, "v9.9.9")
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	cached := filepath.Join(s.Dir, BinaryAsset("amd64"))
	copyTo(t, bin, cached)

	// Pretend the daemon is that very file, then a copy of it.
	s.SelfExe = cached
	if _, err := s.Binary(context.Background(), "amd64"); !strings.Contains(fmt.Sprint(err), ErrOwnExecutable.Error()) {
		t.Fatalf("the daemon's own file: %v", err)
	}
	self := filepath.Join(t.TempDir(), "fleetd")
	copyTo(t, bin, self)
	s.SelfExe = self
	if _, err := s.Binary(context.Background(), "amd64"); !strings.Contains(fmt.Sprint(err), ErrOwnExecutable.Error()) {
		t.Fatalf("a copy of the daemon's executable: %v", err)
	}
}

// releaseServer serves a release's assets, with checksums.txt listing sums.
func releaseServer(t *testing.T, version string, assets map[string][]byte, sums map[string]string) (*httptest.Server, *sync.Map) {
	t.Helper()
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/"+version+"/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		n, _ := hits.LoadOrStore(name, new(int))
		*n.(*int)++
		if name == ChecksumsAsset {
			for asset, sum := range sums {
				fmt.Fprintf(w, "%s  %s\n", sum, asset)
			}
			return
		}
		data, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestBinaryFromTheDaemonsRelease(t *testing.T) {
	bin := program(t, "linux", "amd64", true, "v9.9.9")
	data, _ := os.ReadFile(bin)
	sum := sha256.Sum256(data)
	srv, hits := releaseServer(t, "v9.9.9", map[string][]byte{BinaryAsset("amd64"): data}, map[string]string{BinaryAsset("amd64"): hex.EncodeToString(sum[:])})
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.ReleaseBase = srv.URL + "/"
	// A stale cached binary of the previous version is replaced.
	copyTo(t, program(t, "linux", "amd64", true, "v9.9.8"), filepath.Join(s.Dir, BinaryAsset("amd64")))
	a, err := s.Binary(context.Background(), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != SourceRelease || a.Path != filepath.Join(s.Dir, BinaryAsset("amd64")) || a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("downloaded %+v", a)
	}
	// Now it is the cache's.
	if a, err := s.Binary(context.Background(), "amd64"); err != nil || a.Source != SourceCache {
		t.Fatalf("after the download: %+v %v", a, err)
	}
	if n, _ := hits.Load(BinaryAsset("amd64")); *n.(*int) != 1 {
		t.Fatalf("downloaded %d times", *n.(*int))
	}

	// A download that does not match the release's checksum is refused.
	bad, _ := releaseServer(t, "v9.9.9", map[string][]byte{BinaryAsset("arm64"): []byte("tampered")}, map[string]string{BinaryAsset("arm64"): strings.Repeat("0", 64)})
	s = testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.ReleaseBase = bad.URL + "/"
	if _, err := s.Binary(context.Background(), "arm64"); err == nil || !strings.Contains(err.Error(), "the release lists") {
		t.Fatalf("checksum mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, BinaryAsset("arm64"))); !os.IsNotExist(err) {
		t.Fatal("a mismatched download was kept")
	}
}

// A failed release download is not tried again by the next lookups (every
// bootstrap looks its build up), only once downloadRetry has passed.
func TestFailedDownloadsAreNotRetriedAtOnce(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.ReleaseBase = down.URL + "/"
	for range 3 {
		if _, err := s.Binary(context.Background(), "amd64"); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("an unreachable release: %v", err)
		}
	}
	mu.Lock()
	if asked != 1 {
		t.Fatalf("the release was asked %d times, want once", asked)
	}
	mu.Unlock()

	// A refused asset is not fetched again either.
	other := program(t, "linux", "amd64", true, "v9.9.8")
	data, _ := os.ReadFile(other)
	sum := sha256.Sum256(data)
	srv2, hits2 := releaseServer(t, "v9.9.9", map[string][]byte{BinaryAsset("amd64"): data}, map[string]string{BinaryAsset("amd64"): hex.EncodeToString(sum[:])})
	s = testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.ReleaseBase = srv2.URL + "/"
	for range 2 {
		if _, err := s.Binary(context.Background(), "amd64"); err == nil || !strings.Contains(err.Error(), "v9.9.8") {
			t.Fatalf("a release asset of another version: %v", err)
		}
	}
	if n, _ := hits2.Load(BinaryAsset("amd64")); *n.(*int) != 1 {
		t.Fatalf("the refused asset was downloaded %d times, want once", *n.(*int))
	}

	old := downloadRetry
	downloadRetry = 0
	t.Cleanup(func() { downloadRetry = old })
	_, _ = s.Binary(context.Background(), "amd64")
	if n, _ := hits2.Load(BinaryAsset("amd64")); *n.(*int) != 2 {
		t.Fatalf("after the retry wait: downloaded %d times, want 2", *n.(*int))
	}
}

// A caller that gives up on a slow download leaves it running: the next
// lookup gets the binary without downloading it again.
func TestADownloadOutlivesItsCaller(t *testing.T) {
	bin := program(t, "linux", "amd64", true, "v9.9.9")
	data, _ := os.ReadFile(bin)
	sum := sha256.Sum256(data)
	release := make(chan struct{})
	var mu sync.Mutex
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v9.9.9/" + ChecksumsAsset:
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), BinaryAsset("amd64"))
		case "/v9.9.9/" + BinaryAsset("amd64"):
			mu.Lock()
			downloads++
			mu.Unlock()
			<-release
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.ReleaseBase = srv.URL + "/"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Binary(ctx, "amd64"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a lookup that gave up: %v", err)
	}
	unblock()
	a, err := s.Binary(context.Background(), "amd64")
	if err != nil || a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the next lookup: %+v %v", a, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if downloads != 1 {
		t.Fatalf("downloaded %d times, want once", downloads)
	}
}

// tmuxBundle writes a bundle whose bin/tmux is elfBinary, plus a terminfo
// entry and a hard link to it, as the real bundle has.
func tmuxBundle(t *testing.T, elfBinary string, extra ...tar.Header) string {
	t.Helper()
	data, err := os.ReadFile(elfBinary)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(h tar.Header, body []byte) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	add(tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(tar.Header{Name: "bin/tmux", Typeflag: tar.TypeReg, Mode: 0o755}, data)
	add(tar.Header{Name: "share/terminfo/i/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(tar.Header{Name: "share/terminfo/i/iTerm2.app", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("terminfo"))
	add(tar.Header{Name: "share/terminfo/i/iterm2", Typeflag: tar.TypeLink, Linkname: "share/terminfo/i/iTerm2.app"}, nil)
	for _, h := range extra {
		add(h, nil)
	}
	_ = tw.Close()
	_ = zw.Close()
	p := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTmuxBundle(t *testing.T) {
	amd64 := program(t, "linux", "amd64", false, "")
	env := map[string]string{}
	s := testStore(t, Identity{Version: "v9.9.9"}, env)
	ctx := context.Background()
	if _, err := s.Tmux(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "no tmux bundle") {
		t.Fatalf("no bundle: %v", err)
	}
	cached := filepath.Join(s.Dir, TmuxAsset("amd64"))
	copyTo(t, tmuxBundle(t, amd64), cached)
	a, err := s.Tmux(ctx, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindTmux || a.Source != SourceCache || a.SHA256 != shaOf(t, cached) {
		t.Fatalf("bundle %+v", a)
	}
	if _, err := s.Tmux(ctx, "arm64"); err == nil {
		t.Fatal("served nothing for arm64, but no error")
	}
	copyTo(t, tmuxBundle(t, amd64), filepath.Join(s.Dir, TmuxAsset("arm64")))
	if _, err := s.Tmux(ctx, "arm64"); err == nil || !strings.Contains(err.Error(), "not linux/arm64") {
		t.Fatalf("an amd64 tmux for arm64: %v", err)
	}
	env[EnvTmuxPrefix+"AMD64"] = tmuxBundle(t, amd64, tar.Header{Name: "../escape", Typeflag: tar.TypeReg})
	if _, err := s.Tmux(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "outside its root") {
		t.Fatalf("an escaping entry: %v", err)
	}
	env[EnvTmuxPrefix+"AMD64"] = amd64
	if _, err := s.Tmux(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "not a gzip tmux bundle") {
		t.Fatalf("a binary as a bundle: %v", err)
	}
	if real := os.Getenv("FLEET_TEST_TMUX_BUNDLE"); real != "" {
		if err := checkTmuxBundle(real, "amd64"); err != nil {
			t.Fatalf("the real bundle %s: %v", real, err)
		}
	}
}

func TestOpenNoticesAChangedFile(t *testing.T) {
	bin := program(t, "linux", "amd64", true, "v9.9.9")
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	cached := filepath.Join(s.Dir, BinaryAsset("amd64"))
	copyTo(t, bin, cached)
	a, err := s.Binary(context.Background(), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || int64(len(got)) != a.Size {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	r.Close()

	// make rewrote it in place: same path, other content.
	data, _ := os.ReadFile(cached)
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(cached, data, 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ = a.Open()
	defer r.Close()
	if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("a changed file read as %v", err)
	}
}

func TestStagedCopies(t *testing.T) {
	bin := program(t, "linux", "amd64", true, "v9.9.9")
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	cached := filepath.Join(s.Dir, BinaryAsset("amd64"))
	copyTo(t, bin, cached)
	a, err := s.Binary(context.Background(), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(bin)
	path, size, err := s.Staged(context.Background(), a, true)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != size || size >= a.Size || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("gzip copy %s: %v %v (size %d of %d)", path, fi, err, size, a.Size)
	}
	f, _ := os.Open(path)
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	f.Close()
	if !bytes.Equal(got, want) {
		t.Fatal("the gzip copy does not decompress to the binary")
	}
	if again, _, err := s.Staged(context.Background(), a, true); err != nil || again != path {
		t.Fatalf("second call: %s %v", again, err)
	}
	plain, size, err := s.Staged(context.Background(), a, false)
	if err != nil || size != a.Size || shaOf(t, plain) != a.SHA256 {
		t.Fatalf("plain copy %s: %d bytes, %v", plain, size, err)
	}
	// The copy stays what was looked up when the source is rewritten.
	_ = os.WriteFile(cached, []byte("rewritten"), 0o755)
	if again, _, _ := s.Staged(context.Background(), a, false); shaOf(t, again) != a.SHA256 {
		t.Fatal("the staged copy changed with its source")
	}
	// A copy made from a source that changed since its lookup is refused.
	s2 := testStore(t, s.Self, nil)
	if _, _, err := s2.Staged(context.Background(), a, false); err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("a copy of a changed source: %v", err)
	}

	// Concurrent first calls copy once.
	copyTo(t, bin, cached)
	s3 := testStore(t, s.Self, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s3.Staged(context.Background(), a, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	// Only the newest few copies are kept.
	dir := filepath.Join(s.Dir, ".staged")
	for i := range 8 {
		name := filepath.Join(dir, fmt.Sprintf("%064d", i))
		_ = os.WriteFile(name, nil, 0o600)
		old := time.Now().Add(-time.Duration(10-i) * time.Hour)
		_ = os.Chtimes(name, old, old)
	}
	pruneOldest(dir, "", keepStaged)
	if entries, _ := os.ReadDir(dir); len(entries) != keepStaged {
		t.Fatalf("%d staged copies kept, want %d", len(entries), keepStaged)
	}
}
