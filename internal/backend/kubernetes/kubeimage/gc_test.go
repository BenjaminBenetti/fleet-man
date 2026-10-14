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
		// The manifest stays; the host tag, only there to push from, goes
		// (it would pin every old image on the daemon host).
		tag := "registry.example.com/" + ownRepoPath + ":" + id12(imageID("old"))
		if !h.reg.has(ownRepoPath, digest) || !strings.Contains(h.tools.read(t, "docker.log"), "image rm -- "+tag) {
			t.Errorf("mode %d: manifest kept %v; docker saw:\n%s", tt.mode, h.reg.has(ownRepoPath, digest), h.tools.read(t, "docker.log"))
		}
		ix := h.indexOf(t, tgt.Cluster.ID)
		if len(ix.Images) != 1 || !strings.Contains(ix.Images[0].GCError, tt.why) || ix.Images[0].Tag != "" || *ix.Registries["registry.example.com"].DeleteSupported {
			t.Errorf("mode %d: index %+v", tt.mode, ix)
		}
		// The answer is trusted for a day: the next pass does not ask again,
		// nor untag again, nor look up a login beyond the pull Secret's
		// refresh.
		h.clock.advance(time.Hour)
		lookups := h.creds.lookups()
		if _, err := h.m.GC(context.Background(), tgt, GCOptions{}); err != nil {
			t.Fatal(err)
		}
		if n := h.reg.count("DELETE /v2/" + ownRepoPath + "/manifests/" + zeroDigest); n != 1 {
			t.Errorf("mode %d: DELETE support checked %d times", tt.mode, n)
		}
		if n := strings.Count(h.tools.read(t, "docker.log"), "image rm"); n != 1 || h.creds.lookups() != lookups+1 {
			t.Errorf("mode %d: a later pass untagged %d times, looked up %d logins", tt.mode, n, h.creds.lookups()-lookups)
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
	unlock := h.m.keys.lock(e.lockKey())
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

// TestGCLeavesWhatAnotherConnectionShares: connections of one daemon that
// push to one repository, or load into one local cluster, share the manifest,
// the nodes' copy and the host tag of an image both deliver. The first to
// let go only drops its own record; the last one deletes.
func TestGCLeavesWhatAnotherConnectionShares(t *testing.T) {
	ctx := context.Background()
	t.Run("registry", func(t *testing.T) {
		h := newHarness(t)
		h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
		a := h.target(h.registryCluster(), localcluster.Cluster{})
		b := h.target(h.registryCluster(func(c *fleet.KubeCluster) { c.ID = "prod-xyz789" }), localcluster.Cluster{})
		digest := h.seedPush(t, "shared")
		id := imageID("shared")
		da := h.deliverAndRelease(t, a, id)
		if _, err := h.m.Deliver(ctx, b, builtImage(id), "inst-b"); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(73 * time.Hour)
		res, err := h.m.GC(ctx, a, GCOptions{})
		if err != nil || len(res.Deleted) != 1 || len(h.indexOf(t, a.Cluster.ID).Images) != 0 {
			t.Fatalf("GC of the first connection = %+v, %v", res, err)
		}
		if !h.reg.has(ownRepoPath, digest) || h.reg.count("DELETE /v2/"+ownRepoPath+"/manifests/"+digest) != 0 || strings.Contains(h.tools.read(t, "docker.log"), "image rm") {
			t.Fatalf("deleted what the other connection holds: registry %v; docker:\n%s", h.reg.seen(), h.tools.read(t, "docker.log"))
		}
		if err := h.m.Release(b.Cluster.ID, "inst-b"); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(73 * time.Hour)
		if res, err := h.m.GC(ctx, b, GCOptions{}); err != nil || len(res.Deleted) != 1 {
			t.Fatalf("GC of the last connection = %+v, %v", res, err)
		}
		if h.reg.has(ownRepoPath, digest) || !strings.Contains(h.tools.read(t, "docker.log"), "image rm -- "+da.Tag) {
			t.Errorf("the last connection left the image: registry %v; docker:\n%s", h.reg.seen(), h.tools.read(t, "docker.log"))
		}
	})

	t.Run("node load", func(t *testing.T) {
		h := newHarness(t)
		a := h.target(h.cluster(), minikubeDev)
		b := h.target(h.cluster(func(c *fleet.KubeCluster) { c.ID = "dev-other1"; c.Namespace = "other" }), minikubeDev)
		kind := h.target(h.cluster(func(c *fleet.KubeCluster) { c.ID = "kind-abc123" }), localcluster.Cluster{Kind: localcluster.KinD, Name: "kind"})
		id := imageID("local")
		tag := NodeLoadTag(testDaemonID, id)
		h.deliverAndRelease(t, a, id)
		if _, err := h.m.Deliver(ctx, b, builtImage(id), "inst-b"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.m.Deliver(ctx, kind, builtImage(id), "inst-k"); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(73 * time.Hour)
		if res, err := h.m.GC(ctx, a, GCOptions{}); err != nil || len(res.Deleted) != 1 {
			t.Fatalf("GC = %+v, %v", res, err)
		}
		if l := h.tools.lines(t, "minikube.log"); slices.ContainsFunc(l, func(s string) bool { return strings.Contains(s, "image rm") }) || strings.Contains(h.tools.read(t, "docker.log"), "image rm") {
			t.Fatalf("removed an image another connection holds: minikube %q; docker:\n%s", l, h.tools.read(t, "docker.log"))
		}
		// The other minikube connection goes: the nodes' copy goes with it,
		// but kind's connection still needs the host tag.
		_ = h.m.Release(b.Cluster.ID, "inst-b")
		h.clock.advance(73 * time.Hour)
		if res, err := h.m.GC(ctx, b, GCOptions{}); err != nil || len(res.Deleted) != 1 {
			t.Fatalf("GC = %+v, %v", res, err)
		}
		if l := h.tools.lines(t, "minikube.log"); !slices.Contains(l, "-p dev image rm "+tag) || strings.Contains(h.tools.read(t, "docker.log"), "image rm") {
			t.Errorf("minikube saw %q; docker:\n%s", l, h.tools.read(t, "docker.log"))
		}
	})

	// A delivery on another connection that holds the image's lock while
	// the GC waits for it is seen once the GC gets the lock.
	t.Run("delivery racing the collection", func(t *testing.T) {
		h := newHarness(t)
		a := h.target(h.registryCluster(), localcluster.Cluster{})
		b := h.target(h.registryCluster(func(c *fleet.KubeCluster) { c.ID = "prod-xyz789" }), localcluster.Cluster{})
		digest := h.seedPush(t, "race")
		h.deliverAndRelease(t, a, imageID("race"))
		h.clock.advance(100 * time.Hour)
		e := h.indexOf(t, a.Cluster.ID).Images[0]
		unlock := h.m.keys.lock(e.lockKey())
		done := make(chan *GCResult)
		go func() {
			res, err := h.m.GC(ctx, a, GCOptions{})
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
		time.Sleep(50 * time.Millisecond)
		late := e
		late.Holders, late.ReleasedAt, late.DeliveredAt = []string{"inst-b"}, time.Time{}, h.clock.Now()
		if err := h.m.files.update(h.m.IndexPath(b.Cluster.ID), func(ix *Index) error { ix.Images = append(ix.Images, late); return nil }); err != nil {
			t.Fatal(err)
		}
		unlock()
		<-done
		if !h.reg.has(ownRepoPath, digest) {
			t.Error("the GC deleted a manifest another connection's delivery recorded while it waited")
		}
	})
}

// TestGCRemovesOnlyItsOwnTags: an index entry naming a tag that is not
// fleet's (a hand-edited or foreign index) costs the host and the nodes
// nothing.
func TestGCRemovesOnlyItsOwnTags(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.registryCluster(), minikubeDev)
	old := h.clock.Now().Add(-100 * time.Hour)
	id := imageID("x")
	err := h.m.files.update(h.m.IndexPath(tgt.Cluster.ID), func(ix *Index) error {
		ix.put(Entry{ImageID: id, Strategy: StrategyNodeLoad, Location: minikubeDev.String(), Ref: "busybox:latest", Tag: "busybox:latest", DeliveredAt: old})
		ix.put(Entry{ImageID: id, Strategy: StrategyRegistry, Location: "registry.example.com/" + ownRepoPath, Ref: "x@" + zeroDigest, Digest: zeroDigest, Tag: "registry.example.com/team/app:" + id12(id), DeliveredAt: old})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.m.GC(context.Background(), tgt, GCOptions{})
	if err != nil || len(res.Kept) != 2 {
		t.Fatalf("GC = %+v, %v", res, err)
	}
	for _, e := range res.Kept {
		if !strings.Contains(e.GCError, "not one of this daemon's tags") {
			t.Errorf("kept %+v", e)
		}
	}
	if log := h.tools.read(t, "docker.log") + h.tools.read(t, "minikube.log"); log != "" || len(h.reg.seen()) != 0 {
		t.Errorf("something was removed: %s %v", log, h.reg.seen())
	}
	for tag, want := range map[string]bool{
		NodeLoadTag(testDaemonID, id): true,
		RegistryTag(Registry{Domain: "r.example.com", Prefix: "team"}, testDaemonID, id): true,
		RegistryTag(Registry{Domain: "r.example.com"}, testDaemonID, id):                 true,
		NodeLoadTag(testDaemonID, imageID("other")):                                      false,
		NodeLoadTag("0000000000000000", id):                                              false,
		"fleet.invalid/3fa2c91e/devcontainer@" + zeroDigest:                              false,
		"busybox:" + id12(id):                                                            false,
	} {
		if got := isOwnTag(tag, testDaemonID, id); got != want {
			t.Errorf("isOwnTag(%q) = %v", tag, got)
		}
	}
}

// TestGCGraceFollowsADeliveryAgain: an image unheld for long and delivered
// again is in use again; the GC leaves it through the grace, before its new
// holder takes it.
func TestGCGraceFollowsADeliveryAgain(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.cluster(), minikubeDev)
	img := builtImage(imageID("again"))
	h.deliverAndRelease(t, tgt, img.Config.ID)
	h.clock.advance(100 * time.Hour)
	if _, err := h.m.deliverOnce(context.Background(), tgt, img, StrategyNodeLoad, h.m.IndexPath(tgt.Cluster.ID)); err != nil {
		t.Fatal(err)
	}
	if res, err := h.m.GC(context.Background(), tgt, GCOptions{}); err != nil || len(res.Deleted) != 0 {
		t.Errorf("collected an image delivered again a moment ago: %+v, %v", res, err)
	}
}
