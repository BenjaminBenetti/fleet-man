package devcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
)

type noCredentials struct{}

func (noCredentials) Lookup(context.Context, string) (kubeimage.Credential, error) {
	return kubeimage.Credential{}, nil
}

// offlineRegistry reaches no registry at all.
func offlineRegistry() kubeimage.RemoteOptions {
	return kubeimage.RemoteOptions{
		Credentials: noCredentials{},
		Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: network is unreachable")
		}),
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// oneImageRegistry serves repo:tag as an index over an amd64 and an arm64
// image, each with its own config, and returns options that reach it under
// any registry name, with the arm64 config digest.
func oneImageRegistry(t *testing.T, repo, tag string) (kubeimage.RemoteOptions, string) {
	t.Helper()
	blobs := map[string][]byte{}
	manifests := map[string][]byte{}
	platforms := []map[string]any{}
	var armConfig string
	for _, arch := range []string{"amd64", "arm64"} {
		cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{
			"User": "remote-" + arch, "Env": []string{"PATH=/usr/bin"},
			"Labels": map[string]string{"devcontainer.metadata": `[{"remoteUser":"meta-` + arch + `"}]`},
		}})
		cd := digestOf(cfg)
		blobs[cd] = cfg
		if arch == "arm64" {
			armConfig = cd
		}
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"digest": cd, "size": len(cfg)}, "layers": []any{}})
		md := digestOf(m)
		manifests[md] = m
		platforms = append(platforms, map[string]any{"digest": md, "size": len(m), "platform": map[string]string{"os": "linux", "architecture": arch}})
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": platforms})
	manifests[tag] = idx
	manifests[digestOf(idx)] = idx
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/"+repo+"/")
		if ref, ok := strings.CutPrefix(path, "manifests/"); ok {
			b, found := manifests[ref]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var mt struct {
				MediaType string `json:"mediaType"`
			}
			_ = json.Unmarshal(b, &mt)
			w.Header().Set("Content-Type", mt.MediaType)
			_, _ = w.Write(b)
			return
		}
		if d, ok := strings.CutPrefix(path, "blobs/"); ok && blobs[d] != nil {
			_, _ = w.Write(blobs[d])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return kubeimage.RemoteOptions{
		Credentials: noCredentials{},
		Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			out := r.Clone(r.Context())
			out.URL.Scheme, out.URL.Host, out.Host = "http", host, host
			return http.DefaultTransport.RoundTrip(out)
		}),
	}, armConfig
}

// TestCaptureUpReadsAnImageTheHostLacksFromItsRegistry: an image-only
// configuration whose image the daemon host has not got for the cluster's
// platform is read from its registry, never pulled, so the host's tag of it
// stays on the host's own architecture.
func TestCaptureUpReadsAnImageTheHostLacksFromItsRegistry(t *testing.T) {
	f := newFakeTools(t)
	ws, configFile := appWorkspace(t, nil)
	f.writeConfigs(t, configFile, `,"image":"registry.test/team/base:1"`)
	t.Setenv("FAKE_IMAGE", "registry.test/team/base:1")
	if err := os.Remove(filepath.Join(f.dir, "image.json")); err != nil {
		t.Fatal(err)
	}
	remote, armConfig := oneImageRegistry(t, "team/base", "1")
	jobLog := &syncBuffer{}
	res, err := CaptureUp(backend.WithJobLog(context.Background(), jobLog), CaptureRequest{Tools: f.Tools, WorkspaceDir: ws, Platform: "linux/arm64", Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image.ID != armConfig || res.Image.Architecture != "arm64" || res.Image.Config.User != "remote-arm64" ||
		len(res.Image.RepoDigests) != 1 || !strings.HasPrefix(res.Image.RepoDigests[0], "registry.test/team/base@sha256:") {
		t.Errorf("image %+v", res.Image)
	}
	if log := f.read(t, "docker.log"); strings.Contains(log, "pull") {
		t.Errorf("capture-up pulled:\n%s", log)
	}
	if !strings.Contains(jobLog.String(), "read registry.test/team/base:1 from its registry") {
		t.Errorf("job log:\n%s", jobLog.String())
	}
}
