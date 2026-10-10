package kubeimage

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

// deliverAndRelease delivers a built image for holder and lets it go.
func (h *harness) deliverAndRelease(t *testing.T, tgt Target, id string) *Delivery {
	t.Helper()
	d, err := h.m.Deliver(context.Background(), tgt, builtImage(id), "inst")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Release(tgt.Cluster.ID, "inst"); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGCCollectsPushedImagesPastRetention(t *testing.T) {
	h := newHarness(t)
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	digest := h.seedPush(t, "old")
	d := h.deliverAndRelease(t, tgt, imageID("old"))
	ctx := context.Background()

	h.clock.advance(71 * time.Hour)
	res, err := h.m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted)+len(res.Kept) != 0 {
		t.Fatalf("within retention: %+v, %v", res, err)
	}
	h.clock.advance(2 * time.Hour)
	res, err = h.m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted) != 1 || res.Deleted[0].Ref != d.Ref {
		t.Fatalf("past retention: %+v, %v", res, err)
	}
	if h.reg.has(ownRepoPath, digest) {
		t.Error("the manifest is still in the registry")
	}
	if h.reg.count("DELETE /v2/"+ownRepoPath+"/manifests/"+digest) != 1 || h.reg.count("DELETE /v2/"+ownRepoPath+"/manifests/"+zeroDigest) != 1 {
		t.Errorf("registry saw %v", h.reg.seen())
	}
	if !strings.Contains(h.tools.read(t, "docker.log"), "image rm -- "+d.Tag) {
		t.Errorf("the host tag was not removed:\n%s", h.tools.read(t, "docker.log"))
	}
	ix := h.indexOf(t, tgt.Cluster.ID)
	if len(ix.Images) != 0 || ix.Registries["registry.example.com"].DeleteSupported == nil || !*ix.Registries["registry.example.com"].DeleteSupported {
		t.Errorf("index %+v", ix)
	}
	// The pass refreshed the managed pull Secret.
	if _, ok := h.secrets.get(PullSecretName(testDaemonID, tgt.Cluster.ID)); !ok || len(res.Warnings) != 0 {
		t.Errorf("pull secret not refreshed: %+v", res)
	}
}

func TestGCKeepsWhatTheRegistryCannotDelete(t *testing.T) {
	for _, tt := range []struct {
		mode deleteMode
		why  string
	}{{deleteDisabled, "does not allow deleting images (HTTP 405)"}, {deleteDenied, "may not delete images (HTTP 403)"}} {
		h := newHarness(t)
		h.reg.delete = tt.mode
		tgt := h.target(h.registryCluster(), localcluster.Cluster{})
		digest := h.seedPush(t, "old")
		h.deliverAndRelease(t, tgt, imageID("old"))
		h.clock.advance(73 * time.Hour)

		res, err := h.m.GC(context.Background(), tgt, GCOptions{})
		if err != nil || len(res.Deleted) != 0 || len(res.Kept) != 1 || !strings.Contains(res.Kept[0].GCError, tt.why) {
			t.Fatalf("mode %d: %+v, %v", tt.mode, res, err)
		}
		if !h.reg.has(ownRepoPath, digest) || strings.Contains(h.tools.read(t, "docker.log"), "image rm") {
			t.Errorf("mode %d: something was removed", tt.mode)
		}
		ix := h.indexOf(t, tgt.Cluster.ID)
		if len(ix.Images) != 1 || !strings.Contains(ix.Images[0].GCError, tt.why) || *ix.Registries["registry.example.com"].DeleteSupported {
			t.Errorf("mode %d: index %+v", tt.mode, ix)
		}
		// The answer is trusted for a day: the next pass does not ask again.
		h.clock.advance(time.Hour)
		if _, err := h.m.GC(context.Background(), tgt, GCOptions{}); err != nil {
			t.Fatal(err)
		}
		if n := h.reg.count("DELETE /v2/" + ownRepoPath + "/manifests/" + zeroDigest); n != 1 {
			t.Errorf("mode %d: DELETE support checked %d times", tt.mode, n)
		}
		// Once the registry deletes, the next check after a day collects it.
		h.reg.delete = deleteEnabled
		h.clock.advance(25 * time.Hour)
		res, _ = h.m.GC(context.Background(), tgt, GCOptions{})
		if len(res.Deleted) != 1 || h.reg.has(ownRepoPath, digest) {
			t.Errorf("mode %d: after DELETE was enabled: %+v", tt.mode, res)
		}
	}
}

// TestGCNeverTouchesForeignRepositories: the GC deletes only inside the
// daemon's own repository, whatever the index says.
func TestGCNeverTouchesForeignRepositories(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	digest := h.reg.addManifest("team/app", "v1", mediaOCIManifest, []byte(`{"x":1}`))
	other := h.reg.addManifest("team/fleet/deadbeef/devcontainer", "", mediaOCIManifest, []byte(`{"y":1}`))
	old := h.clock.Now().Add(-100 * time.Hour)
	err := h.m.files.update(h.m.IndexPath(tgt.Cluster.ID), func(ix *Index) error {
		ix.put(Entry{ImageID: imageID("a"), Strategy: StrategyRegistry, Location: "registry.example.com/team/app", Ref: "registry.example.com/team/app@" + digest, Digest: digest, DeliveredAt: old})
		ix.put(Entry{ImageID: imageID("b"), Strategy: StrategyRegistry, Location: "registry.example.com/team/fleet/deadbeef/devcontainer", Ref: "x@" + other, Digest: other, DeliveredAt: old})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.m.GC(context.Background(), tgt, GCOptions{})
	if err != nil || len(res.Kept) != 2 || len(res.Deleted) != 0 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	for _, e := range res.Kept {
		if !strings.Contains(e.GCError, "not this daemon's repository") {
			t.Errorf("kept %+v", e)
		}
	}
	for _, r := range h.reg.seen() {
		if strings.HasPrefix(r, "DELETE") {
			t.Errorf("registry saw %s", r)
		}
	}
}

func TestGCHoldersAndGrace(t *testing.T) {
	h := newHarness(t)
	zero := func(c *fleet.KubeCluster) { c.Image.Retention = "0" }
	tgt := h.target(h.cluster(zero), minikubeDev)
	ctx := context.Background()
	if _, err := h.m.Deliver(ctx, tgt, builtImage(imageID("held")), "keeper"); err != nil {
		t.Fatal(err)
	}
	h.deliverAndRelease(t, tgt, imageID("free"))

	// Retention 0 still waits out the grace.
	res, _ := h.m.GC(ctx, tgt, GCOptions{})
	if len(res.Deleted) != 0 {
		t.Errorf("collected within the grace: %+v", res)
	}
	h.clock.advance(2 * time.Minute)
	res, _ = h.m.GC(ctx, tgt, GCOptions{})
	if len(res.Deleted) != 1 || res.Deleted[0].ImageID != imageID("free") {
		t.Errorf("GC = %+v", res)
	}
	// minikube removes it from the node; fleet's tag goes from the host.
	tag := NodeLoadTag(testDaemonID, imageID("free"))
	if l := h.tools.lines(t, "minikube.log"); !slices.Contains(l, "-p dev image rm "+tag) {
		t.Errorf("minikube saw %q", l)
	}
	if !strings.Contains(h.tools.read(t, "docker.log"), "image rm -- "+tag) {
		t.Errorf("docker saw:\n%s", h.tools.read(t, "docker.log"))
	}

	// A holder Live says is gone is dropped; its image then waits out
	// retention like any other.
	h.clock.advance(time.Hour)
	res, _ = h.m.GC(ctx, tgt, GCOptions{Live: func(holder string) bool { return holder != "keeper" }})
	if !slices.Equal(res.Dropped, []string{"keeper"}) || len(res.Deleted) != 0 {
		t.Errorf("GC with Live = %+v", res)
	}
	h.clock.advance(2 * time.Minute)
	res, _ = h.m.GC(ctx, tgt, GCOptions{})
	if len(res.Deleted) != 1 || len(h.indexOf(t, tgt.Cluster.ID).Images) != 0 {
		t.Errorf("GC = %+v", res)
	}
}

func TestGCOtherStrategies(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	kind := localcluster.Cluster{Kind: localcluster.KinD, Name: "kind"}
	tgt := h.target(h.cluster(), kind)
	h.deliverAndRelease(t, tgt, imageID("k"))
	old := h.clock.Now().Add(-100 * time.Hour)
	gw := &fakeGateway{ok: true}
	h.m.Gateway = gw
	_ = h.m.files.update(h.m.IndexPath(tgt.Cluster.ID), func(ix *Index) error {
		ix.put(Entry{ImageID: imageID("p"), Strategy: StrategyPassthrough, Location: "docker.io/library/ubuntu", Ref: "docker.io/library/ubuntu@" + zeroDigest, DeliveredAt: old})
		ix.put(Entry{ImageID: imageID("g"), Strategy: StrategyGateway, Location: "gateway", Ref: "gw/rid/devcontainer@" + zeroDigest, DeliveredAt: old})
		return nil
	})
	h.clock.advance(73 * time.Hour)
	res, err := h.m.GC(ctx, tgt, GCOptions{})
	if err != nil || len(res.Deleted) != 3 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	// kind cannot remove images from its nodes: only the host tag goes.
	if l := h.tools.lines(t, "kind.log"); len(l) != 1 {
		t.Errorf("kind saw %q", l)
	}
	if !slices.Equal(gw.removed, []string{"gw/rid/devcontainer@" + zeroDigest}) {
		t.Errorf("gateway removed %v", gw.removed)
	}
	if len(h.reg.seen()) != 0 {
		t.Errorf("a passthrough image's registry was asked: %v", h.reg.seen())
	}
}

// TestGCRechecksUnderTheImageLock: a holder that took the image while the
// collection waited for the image's lock keeps it.
func TestGCRechecksUnderTheImageLock(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.cluster(), minikubeDev)
	h.deliverAndRelease(t, tgt, imageID("race"))
	h.clock.advance(100 * time.Hour)
	path := h.m.IndexPath(tgt.Cluster.ID)
	e := h.indexOf(t, tgt.Cluster.ID).Images[0]
	unlock := h.m.keys.lock(path + "|" + e.key())
	done := make(chan *GCResult)
	go func() {
		res, err := h.m.GC(context.Background(), tgt, GCOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	time.Sleep(50 * time.Millisecond)
	_ = h.m.files.update(path, func(ix *Index) error { ix.find(e.key()).hold("late"); return nil })
	unlock()
	if res := <-done; len(res.Deleted) != 0 {
		t.Errorf("collected an image a holder took: %+v", res)
	}
	if ix := h.indexOf(t, tgt.Cluster.ID); len(ix.Images) != 1 || ix.Images[0].Holders[0] != "late" {
		t.Errorf("index %+v", ix.Images)
	}
}

func TestRegistryPrefix(t *testing.T) {
	for in, want := range map[string]string{"fleet/d8/devcontainer": "", "team/fleet/d8/devcontainer": "team", "a/b/fleet/d8/devcontainer": "a/b"} {
		if got := registryPrefix(in); got != want {
			t.Errorf("registryPrefix(%q) = %q", in, got)
		}
	}
}

// TestGCAllCollectsEverythingUnheld: the pass before a connection goes
// collects what retention would still keep, but never a held image.
func TestGCAllCollectsEverythingUnheld(t *testing.T) {
	h := newHarness(t)
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	h.seedPush(t, "fresh")
	h.deliverAndRelease(t, tgt, imageID("fresh"))
	h.seedPush(t, "held")
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(imageID("held")), "keeper"); err != nil {
		t.Fatal(err)
	}
	res, err := h.m.GC(context.Background(), tgt, GCOptions{All: true})
	if err != nil || len(res.Deleted) != 1 || res.Deleted[0].ImageID != imageID("fresh") {
		t.Errorf("GC All = %+v, %v", res, err)
	}
	if ix := h.indexOf(t, tgt.Cluster.ID); len(ix.Images) != 1 || ix.Images[0].ImageID != imageID("held") {
		t.Errorf("index %+v", ix.Images)
	}
}

func TestIndexCallsRefuseBadClusterIDs(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"", "..", "a/b", `a\b`} {
		if err := h.m.Release(id, "x"); err == nil {
			t.Errorf("Release(%q) passed", id)
		}
		if _, err := h.m.Lookup(id, "x"); err == nil {
			t.Errorf("Lookup(%q) passed", id)
		}
		if _, err := h.m.Entries(id); err == nil {
			t.Errorf("Entries(%q) passed", id)
		}
	}
}
