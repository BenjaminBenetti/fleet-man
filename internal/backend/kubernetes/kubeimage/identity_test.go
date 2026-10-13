package kubeimage

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

// seedAttestedPush makes the fake docker's next push report an OCI index
// the fake registry holds, as docker's containerd image store pushes a
// BuildKit build: the platform manifest beside an attestation manifest.
func (h *harness) seedAttestedPush(t *testing.T, platformManifest, attestation string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaOCIIndex, "manifests": []map[string]any{
		{"mediaType": mediaOCIManifest, "digest": platformManifest, "size": 1, "platform": map[string]any{"os": "linux", "architecture": "amd64"}},
		{"mediaType": mediaOCIManifest, "digest": attestation, "size": 1, "platform": map[string]any{"os": "unknown", "architecture": "unknown"},
			"annotations": map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": platformManifest}},
	}})
	d := h.reg.addManifest(ownRepoPath, "", mediaOCIIndex, b)
	h.tools.write(t, "push_digest", d, 0o644)
	return d
}

// Two builds of one content on docker's containerd store are two images
// (index digests) of one platform manifest and config: each delivery
// records the content it delivered, and the index keeps every digest it
// went under while an image of that content remains.
func TestDeliveriesRecordTheContentTheyDelivered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.registryCluster(func(c *fleet.KubeCluster) { c.Image.Retention = "0" })
	tgt := h.target(c, localcluster.Cluster{})
	manifest, config := h.reg.addImage(ownRepoPath, "", "amd64", map[string]any{"Cmd": []string{"sleep", "infinity"}})

	first := h.seedAttestedPush(t, manifest, imageID("provenance of build 1"))
	d1, err := h.m.Deliver(ctx, tgt, builtImage(first), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	second := h.seedAttestedPush(t, manifest, imageID("provenance of build 2"))
	d2, err := h.m.Deliver(ctx, tgt, builtImage(second), "inst-2")
	if err != nil {
		t.Fatal(err)
	}
	if d1.Digest != first || d2.Digest != second || d1.Manifest != manifest || d2.Manifest != manifest || d1.Config != config || d2.Config != config {
		t.Fatalf("deliveries %+v and %+v, want %s and %s of manifest %s, config %s", d1, d2, first, second, manifest, config)
	}
	if n := h.pushes(t); n != 2 {
		t.Fatalf("%d pushes", n)
	}

	configs, delivered, err := h.m.SameContent(c.ID, config, nil)
	if err != nil || !slices.Equal(configs, []string{config}) || !slices.Equal(delivered, []string{first, manifest, second}) {
		t.Fatalf("SameContent(config) = %v %v, %v", configs, delivered, err)
	}
	// A record with only a digest finds its content by it.
	if configs, _, _ := h.m.SameContent(c.ID, "", []string{"sha256:" + "f00d" + first[11:], first}); !slices.Equal(configs, []string{config}) {
		t.Errorf("SameContent(first) configs %v", configs)
	}
	// Another content knows none of them.
	other := imageID("another config")
	if configs, delivered, _ := h.m.SameContent(c.ID, other, []string{zeroDigest}); !slices.Equal(configs, []string{other}) || len(delivered) != 0 {
		t.Errorf("SameContent(other) = %v %v", configs, delivered)
	}
	if configs, delivered, _ := h.m.SameContent(c.ID, "", nil); len(configs)+len(delivered) != 0 {
		t.Errorf("SameContent(nothing) = %v %v", configs, delivered)
	}

	// The first instance is gone and its image collected: the digest it
	// went under stays, as the node that pulled it may still name the
	// content by it.
	if err := h.m.Release(c.ID, "inst-1"); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(2 * time.Minute)
	if res, err := h.m.GC(ctx, tgt, GCOptions{}); err != nil || len(res.Deleted) != 1 || res.Deleted[0].Digest != first {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if _, delivered, _ := h.m.SameContent(c.ID, config, nil); !slices.Equal(delivered, []string{first, manifest, second}) {
		t.Errorf("after the first image's collection: %v", delivered)
	}
	// Once no image of the content is left, neither are its digests.
	if err := h.m.Release(c.ID, "inst-2"); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(2 * time.Minute)
	if res, err := h.m.GC(ctx, tgt, GCOptions{}); err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	if ix := h.indexOf(t, c.ID); len(ix.Images) != 0 || ix.Configs != nil {
		t.Errorf("index %+v", ix)
	}
}

// An unchanged rebuild without attestations is the same image: delivered
// again, it is not pushed, keeps its reference, and an entry an older fleet
// recorded learns what it is.
func TestRedeliveryOfOneImageKeepsItsPinAndLearnsItsContent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.registryCluster()
	tgt := h.target(c, localcluster.Cluster{})
	manifest, config := h.reg.addImage(ownRepoPath, "", "amd64", map[string]any{"Cmd": []string{"sh"}})
	// Without an attestation docker's containerd store pushes the platform
	// manifest itself, whose digest is the image's id.
	h.tools.write(t, "push_digest", manifest, 0o644)
	d1, err := h.m.Deliver(ctx, tgt, builtImage(manifest), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if d1.Digest != manifest || d1.Manifest != manifest || d1.Config != config {
		t.Fatalf("Deliver = %+v", d1)
	}
	// Recorded as an older fleet did: no content.
	if err := h.m.files.update(h.m.IndexPath(c.ID), func(ix *Index) error {
		ix.Images[0].Manifest, ix.Images[0].Config, ix.Configs = "", "", nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d2, err := h.m.Deliver(ctx, tgt, builtImage(manifest), "inst-2")
	if err != nil {
		t.Fatal(err)
	}
	if n := h.pushes(t); n != 1 || d2.Ref != d1.Ref {
		t.Fatalf("%d pushes; second delivery %s, first %s", n, d2.Ref, d1.Ref)
	}
	if d2.Config != config || d2.Manifest != manifest {
		t.Errorf("the redelivered image's content: %+v", d2)
	}
	if _, delivered, _ := h.m.SameContent(c.ID, config, nil); !slices.Equal(delivered, []string{manifest}) {
		t.Errorf("SameContent = %v", delivered)
	}
}

// A registry that cannot say what a pushed digest is (here: a manifest
// that names no config) still gets the image delivered; it is then verified
// by its digests alone.
func TestDeliveryWithoutTheContentStillDelivers(t *testing.T) {
	h := newHarness(t)
	c := h.registryCluster()
	d := h.seedPush(t, "opaque")
	got, err := h.m.Deliver(context.Background(), h.target(c, localcluster.Cluster{}), builtImage(imageID("opaque")), "inst")
	if err != nil || got.Digest != d || got.Config != "" || got.Manifest != "" {
		t.Fatalf("Deliver = %+v, %v", got, err)
	}
	if ix := h.indexOf(t, c.ID); ix.Configs != nil {
		t.Errorf("configs %v", ix.Configs)
	}
}

// A passthrough image's content is its platform's manifest and config in
// its registry, read once per digest and platform.
func TestDeliverPassthroughRecordsItsContent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	amd, amdConfig := h.reg.addImage("devcontainers/base", "", "amd64", map[string]any{"Cmd": []string{"bash"}})
	arm, _ := h.reg.addImage("devcontainers/base", "", "arm64", map[string]any{"Cmd": []string{"bash"}})
	index := h.reg.addIndex("devcontainers/base", "debian-12", map[string]string{"linux/amd64": amd, "linux/arm64/v8": arm})
	img := Image{Ref: "mcr.microsoft.com/devcontainers/base:debian-12", Config: imageConfig(imageID("base"))}
	img.Config.RepoDigests = []string{"mcr.microsoft.com/devcontainers/base@" + index}
	tgt := h.target(h.cluster(), localcluster.Cluster{})
	got, err := h.m.Deliver(ctx, tgt, img, "inst")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != StrategyPassthrough || got.Digest != index || got.Manifest != amd || got.Config != amdConfig {
		t.Fatalf("Deliver = %+v, want index %s of manifest %s, config %s", got, index, amd, amdConfig)
	}
	gets := h.reg.count("GET /v2/devcontainers/base/manifests/")
	again, err := h.m.Deliver(ctx, tgt, img, "inst-2")
	if err != nil || again.Config != amdConfig || again.Manifest != amd {
		t.Fatalf("Deliver again = %+v, %v", again, err)
	}
	if n := h.reg.count("GET /v2/devcontainers/base/manifests/"); n != gets {
		t.Errorf("the content was read again: %d manifest reads, then %d", gets, n)
	}
}

// The gateway serves the platform's manifest itself: that is the manifest
// fleet delivered, beside its config.
func TestDeliverGatewayRecordsItsContent(t *testing.T) {
	h := newHarness(t)
	h.m.Gateway = &fakeGateway{ok: true}
	id := imageID("g")
	got, err := h.m.Deliver(context.Background(), h.target(h.cluster(), localcluster.Cluster{}), builtImage(id), "inst")
	if err != nil || got.Strategy != StrategyGateway || got.Manifest != got.Digest || got.Config != fakeGatewayConfig(id) {
		t.Fatalf("Deliver = %+v, %v", got, err)
	}
}
