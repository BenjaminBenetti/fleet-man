package kubeimage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

const ownRepoPath = "team/fleet/3fa2c91e/devcontainer"

var minikubeDev = localcluster.Cluster{Kind: localcluster.Minikube, Name: "dev"}

// registryCluster is a remote cluster pushing to registry.example.com/team.
func (h *harness) registryCluster(mutate ...func(*fleet.KubeCluster)) *fleet.KubeCluster {
	return h.cluster(append([]func(*fleet.KubeCluster){func(c *fleet.KubeCluster) {
		c.Image.Registry = "registry.example.com/team"
		c.Image.PullSecrets = []string{"team-pull"}
	}}, mutate...)...)
}

// seedPush makes the fake docker's next push report a digest the fake
// registry holds.
func (h *harness) seedPush(t *testing.T, name string) string {
	t.Helper()
	d := h.reg.addManifest(ownRepoPath, "", mediaOCIManifest, []byte(`{"schemaVersion":2,"n":"`+name+`"}`))
	h.tools.write(t, "push_digest", d, 0o644)
	return d
}

func (h *harness) pushes(t *testing.T) int {
	n := 0
	for _, l := range h.tools.lines(t, "docker.log") {
		if strings.HasPrefix(l, "push ") {
			n++
		}
	}
	return n
}

func TestDeliverRegistry(t *testing.T) {
	h := newHarness(t)
	h.creds["registry.example.com"] = Credential{Username: "pusher", Password: "s3cret"}
	c := h.registryCluster()
	tgt := h.target(c, localcluster.Cluster{})
	id := imageID("app")
	d := h.seedPush(t, "app")
	tag := "registry.example.com/" + ownRepoPath + ":" + id12(id)

	var jobLog bytes.Buffer
	ctx := backend.WithJobLog(context.Background(), &jobLog)
	got, err := h.m.Deliver(ctx, tgt, builtImage(id), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	want := &Delivery{
		Strategy: StrategyRegistry, Ref: "registry.example.com/" + ownRepoPath + "@" + d, PullPolicy: PullIfNotPresent,
		PullSecrets: []string{"team-pull", PullSecretName(testDaemonID, c.ID)}, ImageID: id, Digest: d, Tag: tag,
		Location: "registry.example.com/" + ownRepoPath,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Deliver =\n %+v\nwant\n %+v", got, want)
	}
	if log := h.tools.read(t, "docker.log"); !strings.Contains(log, "tag "+id+" "+tag+"\n") || !strings.Contains(log, "push "+tag+"\n") {
		t.Errorf("docker saw:\n%s", log)
	}
	if !strings.Contains(jobLog.String(), "pushing "+tag) || !strings.Contains(jobLog.String(), "by registry") {
		t.Errorf("job log:\n%s", jobLog.String())
	}

	// The managed Secret holds the registry's login and nothing else.
	s, ok := h.secrets.get(PullSecretName(testDaemonID, c.ID))
	if !ok || s.Registry != "registry.example.com" || s.Labels["fleet.dev/cluster"] != c.ID {
		t.Fatalf("pull secret %+v", s)
	}
	var cfg struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(s.DockerConfigJSON, &cfg); err != nil || len(cfg.Auths) != 1 || cfg.Auths["registry.example.com"]["password"] != "s3cret" {
		t.Errorf("pull secret config %s", s.DockerConfigJSON)
	}

	ix := h.indexOf(t, c.ID)
	if len(ix.Images) != 1 || !slices.Equal(ix.Images[0].Holders, []string{"inst-a"}) || ix.Images[0].Ref != want.Ref || !ix.Images[0].DeliveredAt.Equal(h.clock.Now()) {
		t.Errorf("index %+v", ix.Images)
	}

	// Another instance of the same image: the registry still has it, so no
	// second push.
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(id), "inst-b"); err != nil {
		t.Fatal(err)
	}
	if n := h.pushes(t); n != 1 {
		t.Errorf("%d pushes", n)
	}
	if ix := h.indexOf(t, c.ID); !slices.Equal(ix.Images[0].Holders, []string{"inst-a", "inst-b"}) {
		t.Errorf("holders %v", ix.Images[0].Holders)
	}

	// The registry lost it: the next delivery pushes again.
	h.reg.mu.Lock()
	delete(h.reg.manifests[ownRepoPath], d)
	h.reg.mu.Unlock()
	h.seedPush(t, "app-again")
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(id), "inst-c"); err != nil {
		t.Fatal(err)
	}
	if n := h.pushes(t); n != 2 {
		t.Errorf("%d pushes after the registry lost the image", n)
	}
}

// TestDeliverSharesOnePush: instances created from one image at once push
// it once, and every one of them holds it.
func TestDeliverSharesOnePush(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	id := imageID("shared")
	h.seedPush(t, "shared")
	h.tools.write(t, "push_sleep", "0.3", 0o644)
	var wg sync.WaitGroup
	refs := make([]string, 6)
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := h.m.Deliver(context.Background(), tgt, builtImage(id), fmt.Sprintf("inst-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			refs[i] = d.Ref
		}(i)
	}
	wg.Wait()
	if n := h.pushes(t); n != 1 {
		t.Errorf("%d pushes for 6 concurrent deliveries", n)
	}
	ix := h.indexOf(t, tgt.Cluster.ID)
	if len(ix.Images) != 1 || len(ix.Images[0].Holders) != 6 {
		t.Errorf("index %+v", ix.Images)
	}
	for _, r := range refs {
		if r != refs[0] || r == "" {
			t.Errorf("refs %v", refs)
		}
	}
}

func TestDeliverHolderMovesAndRelease(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.cluster(), minikubeDev)
	a, b := imageID("a"), imageID("b")
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(a), "inst"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(a), "other"); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(b), "inst"); err != nil {
		t.Fatal(err)
	}
	ix := h.indexOf(t, tgt.Cluster.ID)
	holders := map[string][]string{}
	for _, e := range ix.Images {
		holders[e.ImageID] = e.Holders
	}
	if !slices.Equal(holders[a], []string{"other"}) || !slices.Equal(holders[b], []string{"inst"}) {
		t.Errorf("holders %v", holders)
	}
	if d, err := h.m.Lookup(tgt.Cluster.ID, "inst"); err != nil || d == nil || d.ImageID != b || d.PullPolicy != PullNever {
		t.Errorf("Lookup = %+v, %v", d, err)
	}
	if d, _ := h.m.Lookup(tgt.Cluster.ID, "nobody"); d != nil {
		t.Errorf("Lookup of nobody = %+v", d)
	}

	h.clock.advance(time.Hour)
	if err := h.m.Release(tgt.Cluster.ID, "other"); err != nil {
		t.Fatal(err)
	}
	for _, e := range h.indexOf(t, tgt.Cluster.ID).Images {
		if e.ImageID == a && (len(e.Holders) != 0 || !e.ReleasedAt.Equal(h.clock.Now())) {
			t.Errorf("released entry %+v", e)
		}
	}
	entries, err := h.m.Entries(tgt.Cluster.ID)
	if err != nil || len(entries) != 2 {
		t.Errorf("Entries = %v, %v", entries, err)
	}
}

// A rebuild holds its new image under a holder of its own; once it
// succeeded the instance's key takes it over and lets its old one go.
func TestMoveHold(t *testing.T) {
	h := newHarness(t)
	tgt := h.target(h.cluster(), minikubeDev)
	old, next := imageID("old"), imageID("next")
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(old), "inst"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(old), "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Deliver(context.Background(), tgt, builtImage(next), "inst@g2"); err != nil {
		t.Fatal(err)
	}
	holders := func() map[string][]string {
		out := map[string][]string{}
		for _, e := range h.indexOf(t, tgt.Cluster.ID).Images {
			out[e.ImageID] = e.Holders
		}
		return out
	}
	if got := holders(); !slices.Equal(got[old], []string{"inst", "other"}) || !slices.Equal(got[next], []string{"inst@g2"}) {
		t.Fatalf("while rebuilding: %v", got)
	}
	// Nothing held by the rebuild's holder (a rolled-back rebuild released
	// it): the key keeps its image.
	if err := h.m.MoveHold(tgt.Cluster.ID, "nobody", "inst"); err != nil {
		t.Fatal(err)
	}
	if got := holders(); !slices.Equal(got[old], []string{"inst", "other"}) {
		t.Fatalf("an empty move changed the holds: %v", got)
	}
	if err := h.m.MoveHold(tgt.Cluster.ID, "inst@g2", "inst"); err != nil {
		t.Fatal(err)
	}
	if got := holders(); !slices.Equal(got[old], []string{"other"}) || !slices.Equal(got[next], []string{"inst"}) {
		t.Fatalf("after the move: %v", got)
	}
	if d, err := h.m.Lookup(tgt.Cluster.ID, "inst"); err != nil || d == nil || d.ImageID != next {
		t.Errorf("Lookup after the move = %+v, %v", d, err)
	}
	if err := h.m.MoveHold(tgt.Cluster.ID, "inst", "inst"); err == nil {
		t.Error("a move onto itself")
	}
}

func TestDeliverNodeLoad(t *testing.T) {
	id := imageID("local")
	tag := "fleet.invalid/3fa2c91e/devcontainer:" + id12(id)
	tests := []struct {
		local localcluster.Cluster
		tool  string
		args  string
	}{
		{minikubeDev, "minikube", "-p dev image load --daemon=true --overwrite=false " + tag},
		{localcluster.Cluster{Kind: localcluster.KinD, Name: "kind"}, "kind", "load docker-image --name kind " + tag},
		{localcluster.Cluster{Kind: localcluster.K3d, Name: "k"}, "k3d", "image import --cluster k " + tag},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			h := newHarness(t)
			c := h.cluster(func(c *fleet.KubeCluster) { c.Image.PullSecrets = []string{"cfg"} })
			got, err := h.m.Deliver(context.Background(), h.target(c, tt.local), builtImage(id), "inst")
			if err != nil {
				t.Fatal(err)
			}
			if got.Strategy != StrategyNodeLoad || got.Ref != tag || got.PullPolicy != PullNever || got.Digest != "" || got.Location != tt.local.String() || !slices.Equal(got.PullSecrets, []string{"cfg"}) {
				t.Errorf("Deliver = %+v", got)
			}
			if l := h.tools.lines(t, tt.tool+".log"); !slices.Equal(l, []string{tt.args}) {
				t.Errorf("%s saw %q", tt.tool, l)
			}
			if !strings.Contains(h.tools.read(t, "docker.log"), "tag "+id+" "+tag) {
				t.Errorf("docker saw:\n%s", h.tools.read(t, "docker.log"))
			}
			// A node load uses no managed pull Secret.
			if len(h.secrets.applied) != 0 {
				t.Errorf("secrets %v", h.secrets.applied)
			}
		})
	}

	t.Run("docker desktop", func(t *testing.T) {
		h := newHarness(t)
		tgt := h.target(h.cluster(), localcluster.Cluster{Kind: localcluster.DockerDesktop, Name: "docker-desktop"})
		if d, err := h.m.Deliver(context.Background(), tgt, builtImage(id), "inst"); err != nil || d.Ref != tag {
			t.Fatalf("Deliver = %+v, %v", d, err)
		}
		if l := h.tools.lines(t, "docker.log"); len(l) != 2 || !strings.HasPrefix(l[1], "image inspect --format {{.Id}} -- "+tag) {
			t.Errorf("docker saw %q", l)
		}
		// The desktop's engine must have the tag: there is nothing to copy
		// it with.
		other := imageID("other")
		h.tools.write(t, "missing", NodeLoadTag(testDaemonID, other)+"\n", 0o644)
		if _, err := h.m.Deliver(context.Background(), tgt, builtImage(other), "inst"); err == nil || !strings.Contains(err.Error(), "share the daemon host's docker") {
			t.Errorf("Deliver of an image the engine lacks = %v", err)
		}
	})

	t.Run("load fails", func(t *testing.T) {
		h := newHarness(t)
		h.tools.write(t, "minikube.fail", "X Exiting due to GUEST_IMAGE_LOAD: boom", 0o644)
		_, err := h.m.Deliver(context.Background(), h.target(h.cluster(), minikubeDev), builtImage(id), "inst")
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeImagePush || !strings.Contains(err.Error(), "GUEST_IMAGE_LOAD") {
			t.Errorf("Deliver = %v", err)
		}
		if ix := h.indexOf(t, "dev-abc123"); len(ix.Images) != 0 {
			t.Errorf("a failed load was recorded: %+v", ix.Images)
		}
	})
}

func TestRedeliver(t *testing.T) {
	ctx := context.Background()
	t.Run("node load", func(t *testing.T) {
		h := newHarness(t)
		tgt := h.target(h.cluster(), minikubeDev)
		id := imageID("x")
		d, err := h.m.Deliver(ctx, tgt, builtImage(id), "inst")
		if err != nil {
			t.Fatal(err)
		}
		if !NeedsRedelivery(d.Strategy, "ErrImageNeverPull", "Container image ... is not present with pull policy of Never") {
			t.Fatal("ErrImageNeverPull does not need a re-delivery")
		}
		h.clock.advance(time.Hour)
		nd, err := h.m.Redeliver(ctx, tgt, *d)
		if err != nil {
			t.Fatal(err)
		}
		if nd.Ref != d.Ref || nd.PullPolicy != PullNever {
			t.Errorf("Redeliver = %+v", nd)
		}
		if l := h.tools.lines(t, "minikube.log"); len(l) != 2 || !strings.Contains(l[1], "--overwrite=true") {
			t.Errorf("minikube saw %q", l)
		}
		ix := h.indexOf(t, tgt.Cluster.ID)
		if !ix.Images[0].DeliveredAt.Equal(h.clock.Now()) || !slices.Equal(ix.Images[0].Holders, []string{"inst"}) {
			t.Errorf("index after the re-delivery %+v", ix.Images[0])
		}

		// The daemon host lost the image: only a rebuild helps.
		h.tools.write(t, "missing", id+"\n", 0o644)
		_, err = h.m.Redeliver(ctx, tgt, *d)
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeImagePull || !strings.Contains(de.Hint, "rebuild") {
			t.Errorf("Redeliver of a lost image = %v", err)
		}
	})

	t.Run("registry", func(t *testing.T) {
		h := newHarness(t)
		h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
		tgt := h.target(h.registryCluster(), localcluster.Cluster{})
		id := imageID("y")
		first := h.seedPush(t, "y1")
		d, err := h.m.Deliver(ctx, tgt, builtImage(id), "inst")
		if err != nil {
			t.Fatal(err)
		}
		second := h.seedPush(t, "y2")
		nd, err := h.m.Redeliver(ctx, tgt, *d)
		if err != nil {
			t.Fatal(err)
		}
		if d.Digest != first || nd.Digest != second || nd.Ref == d.Ref || !slices.Contains(nd.PullSecrets, PullSecretName(testDaemonID, tgt.Cluster.ID)) {
			t.Errorf("Redeliver = %+v (was %+v)", nd, d)
		}
		if n := h.pushes(t); n != 2 {
			t.Errorf("%d pushes", n)
		}
	})

	t.Run("passthrough", func(t *testing.T) {
		h := newHarness(t)
		tgt := h.target(h.cluster(), localcluster.Cluster{})
		_, err := h.m.Redeliver(ctx, tgt, Delivery{Strategy: StrategyPassthrough, Ref: "ubuntu@" + zeroDigest, ImageID: imageID("u")})
		var de *Error
		if !errors.As(err, &de) || de.Code != CodeImagePull {
			t.Errorf("Redeliver = %v", err)
		}
	})
}

func TestNeedsRedelivery(t *testing.T) {
	tests := []struct {
		s       Strategy
		reason  string
		message string
		want    bool
	}{
		{StrategyNodeLoad, "ErrImageNeverPull", "", true},
		{StrategyNodeLoad, "ImagePullBackOff", "not found", false},
		{StrategyRegistry, "ErrImagePull", `failed to pull and unpack image "r/x@sha256:..": failed to resolve reference: r/x@sha256:..: not found`, true},
		{StrategyRegistry, "ImagePullBackOff", "Back-off pulling image: manifest unknown", true},
		{StrategyRegistry, "ImagePullBackOff", "401 Unauthorized", false},
		{StrategyRegistry, "CrashLoopBackOff", "not found", false},
		{StrategyPassthrough, "ErrImagePull", "not found", false},
		{StrategyGateway, "ErrImagePull", "not found", false},
	}
	for _, tt := range tests {
		if got := NeedsRedelivery(tt.s, tt.reason, tt.message); got != tt.want {
			t.Errorf("NeedsRedelivery(%s, %s, %q) = %v", tt.s, tt.reason, tt.message, got)
		}
	}
}

func TestDeliverPassthrough(t *testing.T) {
	ctx := context.Background()
	unbuilt := func(ref string, digests ...string) Image {
		img := Image{Ref: ref, Config: imageConfig(imageID(ref))}
		img.Config.RepoDigests = digests
		return img
	}

	t.Run("public", func(t *testing.T) {
		h := newHarness(t)
		d, _ := h.reg.addImage("devcontainers/base", "debian-12", "amd64", nil)
		img := unbuilt("mcr.microsoft.com/devcontainers/base:debian-12", "mcr.microsoft.com/devcontainers/base@"+d)
		got, err := h.m.Deliver(ctx, h.target(h.cluster(), minikubeDev), img, "inst")
		if err != nil {
			t.Fatal(err)
		}
		if got.Strategy != StrategyPassthrough || got.Ref != "mcr.microsoft.com/devcontainers/base@"+d || got.Digest != d || got.PullPolicy != PullIfNotPresent || len(got.Warnings) != 0 {
			t.Errorf("Deliver = %+v", got)
		}
		if log := h.tools.read(t, "docker.log") + h.tools.read(t, "minikube.log"); log != "" {
			t.Errorf("a passthrough image touched docker or the nodes:\n%s", log)
		}
		if ix := h.indexOf(t, "dev-abc123"); len(ix.Images) != 1 || ix.Images[0].Strategy != StrategyPassthrough || ix.Images[0].Holders[0] != "inst" {
			t.Errorf("index %+v", ix.Images)
		}
	})

	t.Run("digest-pinned run", func(t *testing.T) {
		h := newHarness(t)
		d, _ := h.reg.addImage("org/img", "", "amd64", nil)
		got, err := h.m.Deliver(ctx, h.target(h.cluster(), localcluster.Cluster{}), unbuilt("ghcr.io/org/img@"+d), "inst")
		if err != nil || got.Ref != "ghcr.io/org/img@"+d {
			t.Errorf("Deliver = %+v, %v", got, err)
		}
	})

	t.Run("private", func(t *testing.T) {
		h := newHarness(t)
		h.reg.auth = authBearer
		h.creds["ghcr.io"] = Credential{Username: "u", Password: "p"}
		d, _ := h.reg.addImage("org/private", "v1", "amd64", nil)
		img := unbuilt("ghcr.io/org/private:v1", "ghcr.io/org/private@"+d)

		var jobLog bytes.Buffer
		got, err := h.m.Deliver(backend.WithJobLog(ctx, &jobLog), h.target(h.cluster(), localcluster.Cluster{}), img, "inst")
		if err != nil {
			t.Fatal(err)
		}
		if got.Strategy != StrategyPassthrough || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "needs a login to pull") || !strings.Contains(jobLog.String(), "warning: ") {
			t.Errorf("Deliver = %+v\nlog: %s", got, jobLog.String())
		}
		// Configured pull Secrets cover it.
		got, _ = h.m.Deliver(ctx, h.target(h.cluster(func(c *fleet.KubeCluster) { c.Image.PullSecrets = []string{"ghcr"} }), localcluster.Cluster{}), img, "inst")
		if len(got.Warnings) != 0 || !slices.Equal(got.PullSecrets, []string{"ghcr"}) {
			t.Errorf("with pull secrets: %+v", got)
		}
		// So does a managed Secret, when the image's registry is the
		// connection's.
		c := h.cluster(func(c *fleet.KubeCluster) { c.Image.Registry = "ghcr.io/org" })
		got, _ = h.m.Deliver(ctx, h.target(c, localcluster.Cluster{}), img, "inst")
		if len(got.Warnings) != 0 || !slices.Equal(got.PullSecrets, []string{PullSecretName(testDaemonID, c.ID)}) {
			t.Errorf("with a managed secret: %+v", got)
		}
		// Without the host login it is no passthrough image at all.
		delete(h.creds, "ghcr.io")
		if _, err := h.m.Deliver(ctx, h.target(h.cluster(), localcluster.Cluster{}), img, "inst"); err == nil {
			t.Error("delivered an image neither the registry nor fleet can serve")
		}
	})

	t.Run("local image on the containerd store", func(t *testing.T) {
		h := newHarness(t)
		// docker's containerd store gives a local build a digest too; the
		// registry (Docker Hub here) does not have it.
		img := unbuilt("myimage:dev", "myimage@"+zeroDigest)
		got, err := h.m.Deliver(ctx, h.target(h.cluster(), minikubeDev), img, "inst")
		if err != nil || got.Strategy != StrategyNodeLoad {
			t.Errorf("Deliver = %+v, %v", got, err)
		}
		if h.reg.count("HEAD /v2/library/myimage/manifests/") != 1 {
			t.Errorf("registry requests %v", h.reg.seen())
		}
	})

	t.Run("built images never pass through", func(t *testing.T) {
		h := newHarness(t)
		d, _ := h.reg.addImage("library/vsc-app", "", "amd64", nil)
		img := unbuilt("vsc-app", "vsc-app@"+d)
		img.Built = true
		got, err := h.m.Deliver(ctx, h.target(h.cluster(), minikubeDev), img, "inst")
		if err != nil || got.Strategy != StrategyNodeLoad || len(h.reg.seen()) != 0 {
			t.Errorf("Deliver = %+v, %v; registry saw %v", got, err, h.reg.seen())
		}
	})
}

func TestDeliverNone(t *testing.T) {
	h := newHarness(t)
	_, err := h.m.Deliver(context.Background(), h.target(h.cluster(), localcluster.Cluster{}), builtImage(imageID("x")), "inst")
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeNoDelivery || !strings.HasPrefix(err.Error(), "kubernetes:unsupported:image_delivery: ") || !strings.Contains(err.Error(), "set a registry") || !strings.Contains(err.Error(), "Enable Image Registry") {
		t.Errorf("Deliver = %v", err)
	}
}

func TestDeliverFailures(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	h.tools.write(t, "push_fail", "unauthorized: authentication required", 0o644)
	_, err := h.m.Deliver(ctx, tgt, builtImage(imageID("x")), "inst")
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeImagePush || !strings.Contains(de.Hint, "docker login registry.example.com") {
		t.Errorf("a refused push = %v", err)
	}
	_ = os.Remove(filepath.Join(h.tools.dir, "push_fail"))

	h.tools.write(t, "missing", imageID("gone")+"\n", 0o644)
	_, err = h.m.Deliver(ctx, tgt, builtImage(imageID("gone")), "inst")
	if !errors.As(err, &de) || de.Code != CodeImagePull || !strings.Contains(err.Error(), "no longer on the daemon host") {
		t.Errorf("a lost image = %v", err)
	}

	for name, call := range map[string]func() error{
		"no cluster": func() error {
			_, err := h.m.Deliver(ctx, Target{DaemonID: testDaemonID}, builtImage(imageID("x")), "i")
			return err
		},
		"bad cluster id": func() error {
			_, err := h.m.Deliver(ctx, h.target(h.cluster(func(c *fleet.KubeCluster) { c.ID = "../x" }), minikubeDev), builtImage(imageID("x")), "i")
			return err
		},
		"no holder":   func() error { _, err := h.m.Deliver(ctx, tgt, builtImage(imageID("x")), ""); return err },
		"no image ID": func() error { _, err := h.m.Deliver(ctx, tgt, builtImage("not-an-id"), "i"); return err },
		"no index path": func() error {
			m := &Manager{Docker: h.m.Docker}
			_, err := m.Deliver(ctx, tgt, builtImage(imageID("x")), "i")
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDeliverPullSecretPolicy(t *testing.T) {
	ctx := context.Background()
	name := PullSecretName(testDaemonID, "dev-abc123")
	off := false

	h := newHarness(t)
	h.seedPush(t, "x")
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
	c := h.registryCluster(func(c *fleet.KubeCluster) { c.Image.ManagedPullSecret = &off })
	got, err := h.m.Deliver(ctx, h.target(c, localcluster.Cluster{}), builtImage(imageID("x")), "inst")
	if err != nil || !slices.Equal(got.PullSecrets, []string{"team-pull"}) || len(h.secrets.applied) != 0 || !slices.Contains(h.secrets.deletes, name) {
		t.Errorf("managed secret off: %+v, %v, deletes %v", got, err, h.secrets.deletes)
	}

	// No login on the host: no Secret, and any old one goes.
	h = newHarness(t)
	h.seedPush(t, "x")
	got, err = h.m.Deliver(ctx, h.target(h.registryCluster(), localcluster.Cluster{}), builtImage(imageID("x")), "inst")
	if err != nil || !slices.Equal(got.PullSecrets, []string{"team-pull"}) || !slices.Contains(h.secrets.deletes, name) {
		t.Errorf("no login: %+v, %v", got, err)
	}

	// A login the kubelet cannot use: a warning, and no Secret.
	h = newHarness(t)
	h.seedPush(t, "x")
	h.creds["registry.example.com"] = Credential{IdentityToken: "refresh"}
	got, err = h.m.Deliver(ctx, h.target(h.registryCluster(), localcluster.Cluster{}), builtImage(imageID("x")), "inst")
	if err != nil || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "kubelet cannot use") || len(h.secrets.applied) != 0 {
		t.Errorf("identity token: %+v, %v", got, err)
	}

	// No Secrets to manage it with is a caller error; applying failing
	// fails the delivery.
	h = newHarness(t)
	h.seedPush(t, "x")
	h.creds["registry.example.com"] = Credential{Username: "u", Password: "p"}
	tgt := h.target(h.registryCluster(), localcluster.Cluster{})
	tgt.Secrets = nil
	if _, err := h.m.Deliver(ctx, tgt, builtImage(imageID("x")), "inst"); err == nil {
		t.Error("managed pull secret without Secrets")
	}
	h.secrets.err = errors.New("forbidden: secrets is forbidden")
	if _, err := h.m.Deliver(ctx, h.target(h.registryCluster(), localcluster.Cluster{}), builtImage(imageID("x")), "inst"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("apply failing: %v", err)
	}
}

func TestDeliverGateway(t *testing.T) {
	h := newHarness(t)
	h.m.Gateway = &fakeGateway{ok: true}
	c := h.cluster(func(c *fleet.KubeCluster) { c.Image.PullSecrets = []string{"cfg"} })
	got, err := h.m.Deliver(context.Background(), h.target(c, localcluster.Cluster{}), builtImage(imageID("g")), "inst")
	if err != nil || got.Strategy != StrategyGateway || !strings.HasPrefix(got.Ref, "gw.example.com/rid/devcontainer@") || len(h.secrets.applied) != 0 ||
		!slices.Equal(got.PullSecrets, []string{"cfg", "fleet-gateway"}) {
		t.Errorf("Deliver = %+v, %v", got, err)
	}
	// Looked up again later, the gateway's Secret is still named.
	if d, _ := h.m.Lookup(c.ID, "inst"); d == nil || !slices.Equal(d.PullSecrets, []string{"fleet-gateway"}) {
		t.Errorf("Lookup = %+v", d)
	}
}
