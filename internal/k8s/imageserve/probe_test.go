package imageserve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
)

// The probe image is the same bytes every time for a platform (so it is
// imported once and keeps its digest), differs per platform, imports like
// any `docker save` archive, and is a real image: its config names the diffID
// of its one layer, which holds the one file its command names.
func TestProbeImageIsStableAndReal(t *testing.T) {
	amd, _ := parsePlatform("linux/amd64")
	a1, id1, d1, err := probeArchive(amd)
	if err != nil {
		t.Fatal(err)
	}
	a2, id2, d2, _ := probeArchive(amd)
	if !bytes.Equal(a1, a2) || id1 != id2 || d1 != d2 {
		t.Fatal("the probe image must be the same bytes on every build")
	}
	arm, _ := parsePlatform("linux/arm64")
	if _, _, d3, _ := probeArchive(arm); d3 == d1 {
		t.Fatal("each platform has its own probe manifest")
	}
	if got, err := ProbeDigest("linux/amd64"); err != nil || got != d1 {
		t.Fatalf("ProbeDigest = %q %v, want %q", got, err, d1)
	}

	s := newTestStore(t)
	prep, err := s.Import(context.Background(), bytes.NewReader(a1), id1, "linux/amd64")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if prep.Digest != d1 || prep.MediaType != mediaOCIManifest || prep.Bytes > 4096 {
		t.Fatalf("prepared %+v, want manifest %s and a tiny image", prep, d1)
	}
	var m manifest
	if err := json.Unmarshal(readBlob(t, s, d1), &m); err != nil || len(m.Layers) != 1 || m.Layers[0].MediaType != mediaOCILayerGzip {
		t.Fatalf("manifest %+v (%v)", m, err)
	}
	var cfg struct {
		Architecture string
		OS           string
		Config       struct{ Cmd []string }
		RootFS       struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(readBlob(t, s, m.Config.Digest), &cfg); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(readBlob(t, s, m.Layers[0].Digest)))
	if err != nil {
		t.Fatal(err)
	}
	layer, _ := io.ReadAll(zr)
	if cfg.OS != "linux" || cfg.Architecture != "amd64" || len(cfg.RootFS.DiffIDs) != 1 || cfg.RootFS.DiffIDs[0] != sha256Digest(layer) {
		t.Fatalf("config %+v does not name its layer's diffID %s", cfg, sha256Digest(layer))
	}
	h, err := tar.NewReader(bytes.NewReader(layer)).Next()
	if err != nil || "/"+h.Name != cfg.Config.Cmd[0] {
		t.Fatalf("the layer's file %v (%v), the command %v", h, err, cfg.Config.Cmd)
	}
	for _, d := range []string{d1, m.Config.Digest, m.Layers[0].Digest} {
		if !isProbeDigest(d) {
			t.Errorf("%s is not known as a probe digest", d)
		}
	}
}

// Probe serves the probe image to the connection like a delivered image (a
// reference under the current prefix, the Store's reference, the pull
// Secret only when asked for), imports it once, and its pulls stay out of
// the connection's statistics.
func TestGatewayProbe(t *testing.T) {
	f := newGatewayFixture(t)
	noSecrets := f.target
	noSecrets.Secrets = nil
	p, err := f.gw.Probe(context.Background(), noSecrets)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	want, _ := ProbeDigest("linux/arm64")
	if p.Digest != want || p.Ref != f.tunnel.prefix+"/devcontainer@"+want || p.PullSecret != "" {
		t.Fatalf("probe %+v, want digest %s under %s and no Secret", p, want, f.tunnel.prefix)
	}
	if len(f.secrets.applied) != 0 {
		t.Fatal("a probe without Secrets applied one")
	}
	if !f.gw.Store.Serves("kc-1", p.Digest) || f.docker.saves() != 0 {
		t.Fatalf("serves %v, docker saves %d", f.gw.Store.Serves("kc-1", p.Digest), f.docker.saves())
	}
	if prefix, _ := f.gw.Store.Prefix("kc-1"); prefix != f.tunnel.prefix {
		t.Fatalf("recorded prefix %q", prefix)
	}
	stateBefore, _ := os.ReadFile(f.gw.Store.statePath())

	p2, err := f.gw.Probe(context.Background(), f.target)
	if err != nil || p2.Digest != p.Digest || p2.PullSecret != GatewayPullSecretName(testDaemonID, "kc-1") {
		t.Fatalf("second Probe %+v %v", p2, err)
	}
	if _, user, pass := secretLogin(t, f.secrets.last(t)); user != "kc-1" || pass == "" {
		t.Fatalf("pull Secret login %q", user)
	}
	if stateAfter, _ := os.ReadFile(f.gw.Store.statePath()); !bytes.Equal(stateBefore, stateAfter) {
		t.Fatal("a second probe imported the probe image again")
	}

	// Its pulls are not counted; a real pull is, and each step tells
	// OnPull.
	var told atomic.Int32
	f.gw.OnPull = func(id string) {
		if id == "kc-1" {
			told.Add(1)
		}
	}
	srv := httptest.NewServer(f.gw.Handler())
	t.Cleanup(srv.Close)
	c, _ := f.clusters.Lookup("kc-1")
	pull := func(path string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.SetBasicAuth("kc-1", c.Credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	pull("/v2/devcontainer/manifests/" + p.Digest)
	if _, ok := f.gw.Handler().LastPull("kc-1"); ok || told.Load() != 0 {
		t.Fatal("a probe pull was counted")
	}
	d, err := f.gw.Deliver(context.Background(), f.target, f.image())
	if err != nil {
		t.Fatal(err)
	}
	pull("/v2/devcontainer/manifests/" + d.Digest)
	pull("/v2/devcontainer/blobs/" + f.img.configDigest)
	if st, ok := f.gw.Handler().LastPull("kc-1"); !ok || st.Manifest != d.Digest || st.Bytes != int64(len(f.img.config)) || told.Load() != 2 {
		t.Fatalf("LastPull %+v %v, told %d", st, ok, told.Load())
	}
}

// Without a saved connection there is no credential to serve the probe
// image under.
func TestGatewayProbeNeedsASavedConnection(t *testing.T) {
	f := newGatewayFixture(t)
	draft := f.target
	draft.Cluster = nil
	if _, err := f.gw.Probe(context.Background(), draft); err == nil || !strings.Contains(err.Error(), "saved connection") {
		t.Fatalf("Probe of a draft: %v", err)
	}
	f.tunnel.prefix, f.tunnel.why = "", "Enable Image Registry is off"
	if _, err := f.gw.Probe(context.Background(), f.target); err == nil || !strings.Contains(err.Error(), "Enable Image Registry is off") {
		t.Fatalf("Probe without a registry: %v", err)
	}
}

func TestPullStep(t *testing.T) {
	for kind, want := range map[PullFailureKind]string{
		PullCertificate: StepTLS, PullCredential: StepCredential,
		PullDaemonOffline: StepRoute, PullGatewayUnreachable: StepRoute, PullRegistryOff: StepRoute,
		PullStaleReference: StepDigest, PullDigestUnknown: StepDigest, PullOther: "",
	} {
		if got := PullStep(kind); got != want {
			t.Errorf("PullStep(%s) = %q, want %q", kind, got, want)
		}
	}
}

var _ kubeimage.Gateway = (*Gateway)(nil)
