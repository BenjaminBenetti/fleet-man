package imageserve_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
)

// docker_test.go prepares images from the daemon host's real docker (its
// containerd image store, or the classic overlay2 store, whichever it
// runs) and has that docker pull them back from the image server:
//
//	FLEET_IMAGESERVE_DOCKER=1 go test -run RealDocker -v ./internal/k8s/imageserve/

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// buildImage builds a FROM-scratch image of two layers with unique content
// and returns its ID.
func buildImage(t *testing.T) (id, tag string) {
	t.Helper()
	dir := t.TempDir()
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	tag = "fleet-imageserve-test:" + hex.EncodeToString(nonce)
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("first "+hex.EncodeToString(nonce)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b"), imageserve.RandomBytes(256<<10, uint64(nonce[0])), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY a /a\nCOPY b /b\n"), 0o644)
	dockerOut(t, "build", "-q", "-t", tag, dir)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })
	return dockerOut(t, "image", "inspect", "--format", "{{.Id}}", tag), tag
}

// indexedManifests lists the manifest digests a `docker save` of id names
// in its image index (the containerd store), or nil.
func indexedManifests(t *testing.T, id string) []string {
	t.Helper()
	out, err := exec.Command("docker", "save", id).Output()
	if err != nil {
		t.Fatalf("docker save: %v", err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Size < 1<<20 {
			b, _ := io.ReadAll(tr)
			files[h.Name] = b
		}
	}
	var digests []string
	var walk func(b []byte)
	walk = func(b []byte) {
		var ix struct {
			Manifests []struct {
				MediaType, Digest string
			} `json:"manifests"`
		}
		_ = json.Unmarshal(b, &ix)
		for _, m := range ix.Manifests {
			if strings.Contains(m.MediaType, "index") {
				walk(files["blobs/sha256/"+strings.TrimPrefix(m.Digest, "sha256:")])
			} else {
				digests = append(digests, m.Digest)
			}
		}
	}
	walk(files["index.json"])
	return digests
}

func TestRealDockerPrepareAndPullBack(t *testing.T) {
	if os.Getenv("FLEET_IMAGESERVE_DOCKER") == "" {
		t.Skip("set FLEET_IMAGESERVE_DOCKER=1 to run against the host's docker")
	}
	containerd := strings.Contains(dockerOut(t, "info", "--format", "{{json .DriverStatus}}"), "io.containerd.snapshotter")
	arch := dockerOut(t, "version", "--format", "{{.Server.Arch}}")
	id, _ := buildImage(t)
	s := &imageserve.Store{Dir: t.TempDir()}
	p, err := s.Prepare(context.Background(), id, "linux/"+arch)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Logf("containerd store %v: image %s -> manifest %s (%d bytes to pull)", containerd, id, p.Digest, p.Bytes)
	if containerd {
		// The node-platform manifest of the image index, byte for byte; never
		// the index (the image ID) itself.
		if p.Digest == id {
			t.Fatal("the pod would pin the image index")
		}
		found := false
		for _, d := range indexedManifests(t, id) {
			found = found || d == p.Digest
		}
		if !found {
			t.Fatalf("manifest %s is not one the image index lists", p.Digest)
		}
	}
	if err := s.Ref("kc", p.Digest, id); err != nil {
		t.Fatal(err)
	}

	cl := newClusters("kc")
	cred, _ := cl.EnsureCredential("kc")
	srv := httptest.NewServer(&imageserve.Handler{Store: s, Clusters: cl})
	defer srv.Close()
	fetch := func(path string) ([]byte, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.SetBasicAuth("kc", cred)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b, resp.Header
	}
	mb, h := fetch("/v2/devcontainer/manifests/" + p.Digest)
	var m struct {
		MediaType string
		Config    struct{ Digest string }
		Layers    []struct {
			MediaType, Digest string
			Size              int64
		}
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if h.Get("Content-Type") != m.MediaType {
		t.Fatalf("Content-Type %q, the manifest says %q", h.Get("Content-Type"), m.MediaType)
	}
	cfgBytes, _ := fetch("/v2/devcontainer/blobs/" + m.Config.Digest)
	var cfg struct {
		Architecture string
		RootFS       struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	_ = json.Unmarshal(cfgBytes, &cfg)
	if cfg.Architecture != arch || len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		t.Fatalf("config %+v for %d layers", cfg, len(m.Layers))
	}
	for i, l := range m.Layers {
		b, _ := fetch("/v2/devcontainer/blobs/" + l.Digest)
		if imageserve.Sha256Digest(b) != l.Digest || int64(len(b)) != l.Size {
			t.Fatalf("layer %d does not match its descriptor", i)
		}
		if !strings.HasSuffix(l.MediaType, "gzip") {
			t.Logf("layer %d is %s; not checking its diff_id", i, l.MediaType)
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(zr)
		if imageserve.Sha256Digest(raw) != cfg.RootFS.DiffIDs[i] {
			t.Fatalf("layer %d decompresses to another diff_id", i)
		}
	}

	// The host's docker pulls it back by digest (127.0.0.1 is an insecure
	// registry by default): the API root's challenge, the login, HEAD and GET.
	host := strings.TrimPrefix(srv.URL, "http://")
	cfgDir := t.TempDir()
	login := exec.Command("docker", "--config", cfgDir, "login", "-u", "kc", "--password-stdin", host)
	login.Stdin = strings.NewReader(cred)
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("docker login: %v\n%s", err, out)
	}
	ref := host + "/devcontainer@" + p.Digest
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", ref).Run() })
	// Without the login, docker gets the challenge and is refused (before
	// the image is local under this name: a digest docker already holds
	// may not reach the registry at all).
	if out, err := exec.Command("docker", "--config", t.TempDir(), "pull", ref).CombinedOutput(); err == nil {
		t.Fatalf("a pull without the login worked:\n%s", out)
	}
	pull := exec.Command("docker", "--config", cfgDir, "pull", ref)
	out, err := pull.CombinedOutput()
	if err != nil {
		t.Fatalf("docker pull %s: %v\n%s", ref, err, out)
	}
	if !strings.Contains(string(out), p.Digest) {
		t.Fatalf("docker pull output names no digest:\n%s", out)
	}

	// The image survives the host pruning it.
	dockerOut(t, "image", "rm", "-f", ref)
	if _, err := s.Prepare(context.Background(), id, "linux/"+arch); err != nil {
		t.Fatalf("Prepare after the pull: %v", err)
	}
	var gone *imageserve.ImageGoneError
	if _, err := s.Prepare(context.Background(), imageserve.Sha256Digest([]byte("never built")), "linux/"+arch); !errors.As(err, &gone) {
		t.Fatalf("Prepare of an image docker does not have: %v", err)
	}
}
