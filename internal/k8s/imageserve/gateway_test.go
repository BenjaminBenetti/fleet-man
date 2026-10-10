package imageserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

const testDaemonID = "3fa2c91e0b77d4e5a6b7c8d9e0f1a2b3"

type fakeTunnel struct {
	mu          sync.Mutex
	prefix, why string
	warms       int
}

func (f *fakeTunnel) Registry() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prefix, f.why
}

func (f *fakeTunnel) WarmRegistry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warms++
}

func (f *fakeTunnel) set(prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefix = prefix
}

func (f *fakeTunnel) warmCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.warms
}

type fakeSecrets struct {
	mu      sync.Mutex
	applied []kubeimage.PullSecret
	deleted []string
	// fail fails every apply.
	fail error
}

func (f *fakeSecrets) ApplyPullSecret(_ context.Context, s kubeimage.PullSecret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.applied = append(f.applied, s)
	return nil
}

func (f *fakeSecrets) DeletePullSecret(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeSecrets) last(t *testing.T) kubeimage.PullSecret {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.applied) == 0 {
		t.Fatal("no pull Secret applied")
	}
	return f.applied[len(f.applied)-1]
}

// secretLogin decodes a gateway pull Secret's one login.
func secretLogin(t *testing.T, s kubeimage.PullSecret) (host, user, pass string) {
	t.Helper()
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(s.DockerConfigJSON, &cfg); err != nil || len(cfg.Auths) != 1 {
		t.Fatalf("pull Secret %s: %s (%v)", s.Name, s.DockerConfigJSON, err)
	}
	for h, a := range cfg.Auths {
		if dec, _ := base64.StdEncoding.DecodeString(a.Auth); string(dec) != a.Username+":"+a.Password {
			t.Errorf("auth field %q does not match", a.Auth)
		}
		return h, a.Username, a.Password
	}
	return "", "", ""
}

type fakeConnections struct {
	mu       sync.Mutex
	targets  map[string]kubeimage.Target
	repoints []map[string]string
	fail     error
	called   chan struct{}
}

func (f *fakeConnections) Target(_ context.Context, id string) (kubeimage.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.targets[id]
	if !ok {
		return kubeimage.Target{}, errors.New("no such connection")
	}
	return t, nil
}

func (f *fakeConnections) RepointImages(_ context.Context, _ kubeimage.Target, refs map[string]string) error {
	f.mu.Lock()
	defer func() {
		f.mu.Unlock()
		select {
		case f.called <- struct{}{}:
		default:
		}
	}()
	if f.fail != nil {
		return f.fail
	}
	f.repoints = append(f.repoints, refs)
	return nil
}

// gatewayFixture is a Gateway over a scripted docker holding one image.
type gatewayFixture struct {
	gw       *Gateway
	tunnel   *fakeTunnel
	clusters *fakeClusters
	secrets  *fakeSecrets
	docker   *fakeDocker
	target   kubeimage.Target
	img      testImage
	imageID  string
	clock    *fakeClock
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	clock := &fakeClock{t: time.Now()}
	docker := newFakeDocker(t)
	img := newTestImage(t, "arm64", "v8", true, layerTar(t, map[string]string{"bin/app": "arm"}))
	amd := newTestImage(t, "amd64", "", true, layerTar(t, map[string]string{"bin/app": "amd"}))
	archive, id := containerdArchive(t, []testImage{amd, img})
	docker.add(t, id, archive)
	f := &gatewayFixture{
		tunnel:   &fakeTunnel{prefix: "gw.example.com/" + strings.Repeat("a", 64)},
		clusters: newFakeClusters(Cluster{ID: "kc-1", PacePercent: 80}),
		secrets:  &fakeSecrets{},
		docker:   docker,
		img:      img,
		imageID:  id,
		clock:    clock,
	}
	f.target = kubeimage.Target{
		Cluster:  &fleet.KubeCluster{ID: "kc-1", Namespace: "dev", Image: fleet.KubeImageSettings{Mode: fleet.KubeImageGateway, PullSecrets: []string{"regcred"}}},
		DaemonID: testDaemonID,
		Platform: "linux/arm64",
		Secrets:  f.secrets,
		Labels:   map[string]string{"fleet.dev/owner": "3fa2c91e0b77"},
	}
	f.gw = &Gateway{Store: &Store{Dir: t.TempDir(), Docker: docker.path(), Now: clock.now}, Clusters: f.clusters, Tunnel: f.tunnel}
	return f
}

func (f *gatewayFixture) image() kubeimage.Image {
	return kubeimage.Image{Ref: "vsc-app-1234", Config: kubeshim.ImageConfig{ID: f.imageID}, Built: true}
}

func TestGatewayAvailable(t *testing.T) {
	if ok, why := (&Gateway{}).Available(kubeimage.Target{}); ok || why == "" {
		t.Fatalf("an unwired gateway: %v %q", ok, why)
	}
	f := newGatewayFixture(t)
	if ok, why := f.gw.Available(f.target); !ok {
		t.Fatalf("available: %q", why)
	}
	f.tunnel.prefix, f.tunnel.why = "", "Enable Image Registry is off"
	if ok, why := f.gw.Available(f.target); ok || why != "Enable Image Registry is off" {
		t.Fatalf("no registry: %v %q", ok, why)
	}
	_, err := f.gw.Deliver(context.Background(), f.target, f.image())
	var kerr *kubeimage.Error
	if !errors.As(err, &kerr) || kerr.Code != kubeimage.CodeNoDelivery || !strings.Contains(err.Error(), "Enable Image Registry is off") {
		t.Fatalf("Deliver without a registry: %v", err)
	}
}

func TestGatewayDeliver(t *testing.T) {
	f := newGatewayFixture(t)
	d, err := f.gw.Deliver(context.Background(), f.target, f.image())
	if err != nil {
		t.Fatal(err)
	}
	prefix, _ := f.tunnel.Registry()
	// The node-platform manifest (arm64 here), never the image (index) ID.
	if d.Digest != f.img.manifestDigest || d.Ref != prefix+"/devcontainer@"+f.img.manifestDigest {
		t.Fatalf("delivery %+v, want %s", d, f.img.manifestDigest)
	}
	if d.PullSecret != "fleet-gwpull-3fa2c91e0b77-kc-1" || d.Location != "gateway" {
		t.Fatalf("delivery %+v", d)
	}
	s := f.secrets.last(t)
	host, user, pass := secretLogin(t, s)
	if s.Name != d.PullSecret || host != "gw.example.com" || user != "kc-1" || pass == "" || s.Labels["fleet.dev/owner"] != "3fa2c91e0b77" {
		t.Fatalf("pull Secret %+v (login %s %s)", s, host, user)
	}
	if !f.gw.Store.Serves("kc-1", d.Digest) {
		t.Fatal("the connection does not serve its delivery")
	}
	if f.tunnel.warmCount() != 1 {
		t.Fatalf("WarmRegistry called %d times", f.tunnel.warmCount())
	}
	if got, _ := f.gw.Store.Prefix("kc-1"); got != prefix {
		t.Fatalf("recorded prefix %q", got)
	}

	// The Secret's login pulls the image from the handler.
	srv := httptest.NewServer(f.gw.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/devcontainer/manifests/"+d.Digest, nil)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("pull with the Secret's login: %v %v", resp, err)
	}
	resp.Body.Close()

	// Delivering again neither saves again nor mints another credential.
	if _, err := f.gw.Deliver(context.Background(), f.target, f.image()); err != nil {
		t.Fatal(err)
	}
	if f.docker.saves() != 1 {
		t.Fatalf("docker save ran %d times", f.docker.saves())
	}
	if _, _, pass2 := secretLogin(t, f.secrets.last(t)); pass2 != pass {
		t.Fatal("a second delivery changed the credential")
	}
}

func TestGatewayDeliverFailures(t *testing.T) {
	f := newGatewayFixture(t)
	f.docker.remove(f.imageID)
	_, err := f.gw.Deliver(context.Background(), f.target, f.image())
	var kerr *kubeimage.Error
	if !errors.As(err, &kerr) || kerr.Code != kubeimage.CodeImagePull || !strings.Contains(kerr.Hint, "rebuild") {
		t.Fatalf("an image the host lost: %v", err)
	}

	f = newGatewayFixture(t)
	f.target.Platform = "linux/s390x"
	if _, err := f.gw.Deliver(context.Background(), f.target, f.image()); !errors.As(err, &kerr) || kerr.Code != kubeimage.CodeImagePush || !strings.Contains(err.Error(), "linux/s390x") {
		t.Fatalf("an image for other platforms: %v", err)
	}

	f = newGatewayFixture(t)
	f.target.Secrets = nil
	if _, err := f.gw.Deliver(context.Background(), f.target, f.image()); err == nil {
		t.Fatal("delivered with nowhere to keep the pull Secret")
	}

	// A delivery that fails after the export leaves the connection serving
	// nothing: kubeimage records no entry, so its GC would never let go.
	f = newGatewayFixture(t)
	f.secrets.fail = errors.New("secrets is forbidden")
	if _, err := f.gw.Deliver(context.Background(), f.target, f.image()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a failed pull Secret: %v", err)
	}
	if f.gw.Store.Serves("kc-1", f.img.manifestDigest) {
		t.Fatal("a failed delivery left the connection holding the image")
	}
}

func TestGatewayThroughKubeimage(t *testing.T) {
	f := newGatewayFixture(t)
	f.target.Cluster.Image.Retention = "0"
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw, Now: f.clock.now}

	if res := m.Resolve(f.target); res.Strategy != kubeimage.StrategyGateway {
		t.Fatalf("Resolve = %+v", res)
	}
	d, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Strategy != kubeimage.StrategyGateway || d.Digest != f.img.manifestDigest || d.PullPolicy != kubeimage.PullIfNotPresent {
		t.Fatalf("delivery %+v", d)
	}
	if want := []string{"regcred", "fleet-gwpull-3fa2c91e0b77-kc-1"}; strings.Join(d.PullSecrets, ",") != strings.Join(want, ",") {
		t.Fatalf("pull secrets %v, want %v", d.PullSecrets, want)
	}
	if got, _ := m.Lookup("kc-1", "inst-1"); got == nil || got.Ref != d.Ref {
		t.Fatalf("Lookup = %+v", got)
	}

	// Held: GC keeps it.
	f.clock.advance(time.Hour)
	if res, err := m.GC(context.Background(), f.target, kubeimage.GCOptions{}); err != nil || len(res.Deleted) != 0 {
		t.Fatalf("GC of a held image: %+v, %v", res, err)
	}
	// Released past retention: the connection stops serving it, and the
	// cache drops it at the next sweep.
	if err := m.Release("kc-1", "inst-1"); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Minute)
	res, err := m.GC(context.Background(), f.target, kubeimage.GCOptions{})
	if err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC: %+v, %v", res, err)
	}
	if f.gw.Store.Serves("kc-1", d.Digest) {
		t.Fatal("a collected image is still served")
	}
	f.clock.advance(2 * sweepGrace)
	if _, err := f.gw.Store.Sweep(); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.gw.Store.Size(); n != 0 {
		t.Fatalf("the cache keeps %d bytes of a collected image", n)
	}
}

func TestGatewayForgetAndRotate(t *testing.T) {
	f := newGatewayFixture(t)
	d, err := f.gw.Deliver(context.Background(), f.target, f.image())
	if err != nil {
		t.Fatal(err)
	}
	_, _, old := secretLogin(t, f.secrets.last(t))
	applied := len(f.secrets.applied)

	// Rotation rewrites the credential inside the same Secret: applied
	// again, never deleted.
	if err := f.gw.RotateCredential(context.Background(), f.target); err != nil {
		t.Fatal(err)
	}
	s := f.secrets.last(t)
	_, _, cred := secretLogin(t, s)
	if len(f.secrets.applied) != applied+1 || len(f.secrets.deleted) != 0 || s.Name != d.PullSecret || cred == old {
		t.Fatalf("rotation: applied %d (was %d), deleted %v, same credential %v", len(f.secrets.applied), applied, f.secrets.deleted, cred == old)
	}
	if c, _ := f.clusters.Lookup("kc-1"); c.Credential != cred || c.Previous != old {
		t.Fatalf("after rotation the connection holds %+v", c)
	}

	if err := f.gw.Forget(context.Background(), f.target); err != nil {
		t.Fatal(err)
	}
	if f.gw.Store.Serves("kc-1", d.Digest) {
		t.Fatal("a forgotten connection still serves")
	}
	if len(f.secrets.deleted) != 1 || f.secrets.deleted[0] != d.PullSecret {
		t.Fatalf("deleted %v", f.secrets.deleted)
	}
}

func TestGatewayRepointsOnARegistryChange(t *testing.T) {
	f := newGatewayFixture(t)
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw}
	conns := &fakeConnections{targets: map[string]kubeimage.Target{"kc-1": f.target}, called: make(chan struct{}, 4)}
	f.gw.Connections, f.gw.Index = conns, m
	d, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	oldRef := d.Ref

	waitCall := func() {
		t.Helper()
		select {
		case <-conns.called:
		case <-time.After(5 * time.Second):
			t.Fatal("no re-point")
		}
	}

	// The same prefix again (a reconnect) changes nothing.
	f.gw.RegistryChanged(f.tunnel.prefix)
	f.gw.RegistryChanged("")
	time.Sleep(50 * time.Millisecond)
	if len(conns.repoints) != 0 {
		t.Fatalf("re-pointed on an unchanged prefix: %v", conns.repoints)
	}

	// A new host, with the cluster failing at first: the prefix stays, and
	// the next registration tries again.
	newPrefix := "gw2.example.com:8443/" + strings.Repeat("b", 64)
	f.tunnel.set(newPrefix)
	conns.fail = errors.New("apiserver down")
	f.gw.RegistryChanged(newPrefix)
	waitCall()
	f.gw.repointMu.Lock() // the failed pass is over
	f.gw.repointMu.Unlock()
	if got, _ := f.gw.Store.Prefix("kc-1"); got == newPrefix {
		t.Fatal("a failed re-point recorded the new prefix")
	}
	conns.mu.Lock()
	conns.fail = nil
	conns.mu.Unlock()
	f.gw.RegistryChanged(newPrefix)
	waitCall()
	f.gw.repointMu.Lock()
	f.gw.repointMu.Unlock()

	want := Ref(newPrefix, d.Digest)
	if len(conns.repoints) != 1 || conns.repoints[0][oldRef] != want {
		t.Fatalf("re-points %v, want %s -> %s", conns.repoints, oldRef, want)
	}
	if got, _ := m.Lookup("kc-1", "inst-1"); got == nil || got.Ref != want {
		t.Fatalf("the index still names %+v", got)
	}
	if host, _, _ := secretLogin(t, f.secrets.last(t)); host != "gw2.example.com:8443" {
		t.Fatalf("the pull Secret is for %s", host)
	}
	if got, _ := f.gw.Store.Prefix("kc-1"); got != newPrefix {
		t.Fatalf("recorded prefix %q", got)
	}
}

func TestClassifyPull(t *testing.T) {
	f := newGatewayFixture(t)
	d, err := f.gw.Deliver(context.Background(), f.target, f.image())
	if err != nil {
		t.Fatal(err)
	}
	prefix, _ := f.tunnel.Registry()
	unknown := Ref(prefix, sha256Digest([]byte("gone")))
	stale := Ref("gw.example.com/"+strings.Repeat("c", 64), d.Digest)
	resolveErr := func(ref, tail string) string {
		return `Failed to pull image "` + ref + `": rpc error: code = NotFound desc = failed to pull and unpack image "` + ref + `": failed to resolve reference "` + ref + `": ` + tail
	}
	for _, c := range []struct {
		name, ref, reason, msg string
		want                   PullFailureKind
		redeliver              bool
	}{
		{"502 while reconnecting", d.Ref, "ErrImagePull", resolveErr(d.Ref, "unexpected status from HEAD request to https://gw.example.com/v2/x/devcontainer/manifests/"+d.Digest+": 502 Bad Gateway"), PullDaemonOffline, false},
		{"404 after the session was reaped", d.Ref, "ImagePullBackOff", `Back-off pulling image "` + d.Ref + `": ErrImagePull: ` + resolveErr(d.Ref, d.Ref+": not found"), PullDaemonOffline, false},
		{"gateway stopped", d.Ref, "ErrImagePull", resolveErr(d.Ref, `failed to do request: Head "https://gw.example.com/v2/x/devcontainer/manifests/`+d.Digest+`": dial tcp 192.168.49.1:443: connect: connection refused`), PullGatewayUnreachable, false},
		{"no route", d.Ref, "ErrImagePull", resolveErr(d.Ref, "dial tcp: lookup gw.example.com: no such host"), PullGatewayUnreachable, false},
		{"untrusted certificate", d.Ref, "ErrImagePull", resolveErr(d.Ref, "tls: failed to verify certificate: x509: certificate signed by unknown authority"), PullCertificate, false},
		{"credential", d.Ref, "ErrImagePull", resolveErr(d.Ref, "unexpected status from HEAD request to https://gw.example.com/v2/x: 401 Unauthorized"), PullCredential, false},
		{"digest the daemon does not serve", unknown, "ErrImagePull", resolveErr(unknown, unknown+": not found"), PullDigestUnknown, true},
		{"old registry id", stale, "ErrImagePull", resolveErr(stale, stale+": not found"), PullStaleReference, false},
		{"something else", d.Ref, "ErrImagePull", "rpc error: code = Unknown desc = disk full", PullOther, false},
	} {
		got, ok := f.gw.ClassifyPull("kc-1", c.ref, c.reason, c.msg)
		if !ok || got.Kind != c.want || got.Redeliver != c.redeliver || got.Message == "" {
			t.Errorf("%s: %+v, %v; want %s", c.name, got, ok, c.want)
		}
		if c.want == PullDaemonOffline && !strings.Contains(got.Message, "daemon was offline") {
			t.Errorf("%s: message %q does not say the daemon was offline", c.name, got.Message)
		}
	}
	if _, ok := f.gw.ClassifyPull("kc-1", d.Ref, "CrashLoopBackOff", "back-off"); ok {
		t.Error("a non-pull reason was classified")
	}

	// With the registry off (the toggle, the tunnel) the gateway 404s the
	// session like a reaped one, but the daemon is up and knows why.
	f.tunnel.mu.Lock()
	f.tunnel.prefix, f.tunnel.why = "", "Enable Image Registry is off"
	f.tunnel.mu.Unlock()
	got, ok := f.gw.ClassifyPull("kc-1", d.Ref, "ErrImagePull", resolveErr(d.Ref, d.Ref+": not found"))
	if !ok || got.Kind != PullRegistryOff || !strings.Contains(got.Message, "Enable Image Registry is off") || got.Redeliver {
		t.Errorf("a pull while the registry is off: %+v, %v", got, ok)
	}
}

func TestGatewayDeliverAfterAPrefixChangeRepointsTheOlderImages(t *testing.T) {
	// A delivery that meets a new prefix before the registration's change
	// reached the re-point must not mark the connection re-pointed: its older
	// references still name the old prefix.
	f := newGatewayFixture(t)
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw}
	conns := &fakeConnections{targets: map[string]kubeimage.Target{"kc-1": f.target}, called: make(chan struct{}, 4)}
	f.gw.Connections, f.gw.Index = conns, m
	old, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	newPrefix := "gw.example.com/" + strings.Repeat("e", 64)
	f.tunnel.set(newPrefix)

	second := newTestImage(t, "arm64", "v8", true, layerTar(t, map[string]string{"bin/app": "v2"}))
	archive, id := containerdArchive(t, []testImage{second})
	f.docker.add(t, id, archive)
	d, err := m.Deliver(context.Background(), f.target, kubeimage.Image{Ref: "vsc-app-v2", Config: kubeshim.ImageConfig{ID: id}, Built: true}, "inst-2")
	if err != nil {
		t.Fatal(err)
	}
	if d.Ref != Ref(newPrefix, second.manifestDigest) {
		t.Fatalf("the new delivery names %s", d.Ref)
	}
	select {
	case <-conns.called:
	case <-time.After(5 * time.Second):
		t.Fatal("the older image was not re-pointed")
	}
	f.gw.repointMu.Lock()
	f.gw.repointMu.Unlock()
	if len(conns.repoints) != 1 || conns.repoints[0][old.Ref] != Ref(newPrefix, old.Digest) {
		t.Fatalf("re-points %v, want %s -> the new prefix", conns.repoints, old.Ref)
	}
	if got, _ := f.gw.Store.Prefix("kc-1"); got != newPrefix {
		t.Fatalf("recorded prefix %q", got)
	}
}

// lockedClock is a fake clock that timers may read.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGatewaySweepsWhatTheGCLetGo(t *testing.T) {
	// The sweep a GC pass runs keeps what the pass just let go for
	// sweepGrace, and later passes sweep only when they collect something
	// too: a follow-up sweep must take it.
	f := newGatewayFixture(t)
	clock := &lockedClock{t: time.Unix(1_800_000_000, 0)}
	f.gw.Store.Now = clock.now
	f.gw.sweepDelay = time.Hour // fired by hand below
	f.target.Cluster.Image.Retention = "0"
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw, Now: clock.now}
	if _, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("kc-1", "inst-1"); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Minute)
	if res, err := m.GC(context.Background(), f.target, kubeimage.GCOptions{}); err != nil || len(res.Deleted) != 1 {
		t.Fatalf("GC: %+v, %v", res, err)
	}
	if n, _ := f.gw.Store.Size(); n == 0 {
		t.Fatal("the GC pass swept inside the grace")
	}
	// Hours of passes with nothing to collect sweep nothing.
	clock.advance(time.Hour)
	if _, err := m.GC(context.Background(), f.target, kubeimage.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	f.gw.sweepMu.Lock()
	timer := f.gw.sweepTimer
	f.gw.sweepMu.Unlock()
	if timer == nil {
		t.Fatal("no follow-up sweep was armed")
	}
	timer.Reset(time.Millisecond)
	waitFor(t, "the follow-up sweep", func() bool { n, _ := f.gw.Store.Size(); return n == 0 })

	// One that meets an import tries again later.
	f.gw.sweepMu.Lock()
	f.gw.sweepTimer.Stop()
	f.gw.sweepTimer = nil
	f.gw.sweepMu.Unlock()
	f.gw.Store.gc.RLock()
	f.gw.sweepNow()
	f.gw.Store.gc.RUnlock()
	f.gw.sweepMu.Lock()
	rearmed := f.gw.sweepTimer != nil
	f.gw.sweepMu.Unlock()
	if !rearmed {
		t.Fatal("a sweep that met an import was not tried again")
	}
}

// slowSave wraps the fixture's docker so `docker save` waits for release():
// the export of a big image, during which the gateway may change.
func slowSave(t *testing.T, f *gatewayFixture) (started <-chan struct{}, release func()) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = save ]; then\n\t: > '" + dir + "/started'\n\twhile [ ! -f '" + dir + "/go' ]; do sleep 0.01; done\nfi\nexec '" + f.docker.path() + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.gw.Store.Docker = filepath.Join(dir, "docker")
	ch := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
				close(ch)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return ch, func() { _ = os.WriteFile(filepath.Join(dir, "go"), nil, 0o600) }
}

func TestGatewayPrefixChangeDuringAnExport(t *testing.T) {
	// The gateway hands out a new registry id while a big image exports:
	// the delivery names the new one and must not re-point the connection
	// back to the old.
	f := newGatewayFixture(t)
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw}
	conns := &fakeConnections{targets: map[string]kubeimage.Target{"kc-1": f.target}, called: make(chan struct{}, 8)}
	f.gw.Connections, f.gw.Index = conns, m
	first, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	oldPrefix, _ := f.tunnel.Registry()

	second := newTestImage(t, "arm64", "v8", true, layerTar(t, map[string]string{"bin/app": "v2"}))
	archive, id := containerdArchive(t, []testImage{second})
	f.docker.add(t, id, archive)
	started, release := slowSave(t, f)
	type result struct {
		d   *kubeimage.Delivery
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := m.Deliver(context.Background(), f.target, kubeimage.Image{Ref: "vsc-app-v2", Config: kubeshim.ImageConfig{ID: id}, Built: true}, "inst-2")
		done <- result{d, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("docker save never ran")
	}
	// The registration's change, re-pointed while the export runs.
	newPrefix := "gw.example.com/" + strings.Repeat("f", 64)
	f.tunnel.set(newPrefix)
	f.gw.repoint(context.Background(), newPrefix)
	release()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	time.Sleep(50 * time.Millisecond) // any re-point the delivery set off
	f.gw.repointMu.Lock()
	f.gw.repointMu.Unlock()

	if r.d.Ref != Ref(newPrefix, second.manifestDigest) {
		t.Fatalf("the delivery names %s, want the new prefix", r.d.Ref)
	}
	if got, _ := f.gw.Store.Prefix("kc-1"); got != newPrefix {
		t.Fatalf("recorded prefix %q, want %q", got, newPrefix)
	}
	if got, _ := m.Lookup("kc-1", "inst-1"); got == nil || got.Ref != Ref(newPrefix, first.Digest) {
		t.Fatalf("the older image's entry names %+v", got)
	}
	conns.mu.Lock()
	defer conns.mu.Unlock()
	for _, refs := range conns.repoints {
		for from, to := range refs {
			if strings.HasPrefix(to, oldPrefix+"/") {
				t.Errorf("re-pointed %s back to the old prefix (%s)", from, to)
			}
		}
	}
	if host, _, _ := secretLogin(t, f.secrets.last(t)); host != "gw.example.com" {
		t.Errorf("the pull Secret is for %s", host)
	}

	// A pass for a prefix the tunnel has since replaced does nothing.
	n := len(conns.repoints)
	conns.mu.Unlock()
	f.gw.repoint(context.Background(), oldPrefix)
	conns.mu.Lock()
	if len(conns.repoints) != n {
		t.Errorf("a pass for the old prefix re-pointed: %v", conns.repoints[n:])
	}
	if got, _ := f.gw.Store.Prefix("kc-1"); got != newPrefix {
		t.Errorf("a pass for the old prefix recorded %q", got)
	}
}

func TestGatewayRetriesAFailedRepoint(t *testing.T) {
	// The tunnel reports a prefix only when it changes, so a re-point that
	// met an unreachable cluster is retried by the gateway itself.
	f := newGatewayFixture(t)
	idx := t.TempDir()
	m := &kubeimage.Manager{IndexPath: func(id string) string { return filepath.Join(idx, id, "images.json") }, Gateway: f.gw}
	conns := &fakeConnections{targets: map[string]kubeimage.Target{"kc-1": f.target}, called: make(chan struct{}, 8), fail: errors.New("apiserver down")}
	f.gw.Connections, f.gw.Index = conns, m
	f.gw.retryMin = 20 * time.Millisecond
	d, err := m.Deliver(context.Background(), f.target, f.image(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	newPrefix := "gw.example.com/" + strings.Repeat("9", 64)
	f.tunnel.set(newPrefix)
	f.gw.RegistryChanged(newPrefix)
	select {
	case <-conns.called:
	case <-time.After(5 * time.Second):
		t.Fatal("no re-point")
	}
	conns.mu.Lock()
	conns.fail = nil
	conns.mu.Unlock()
	waitFor(t, "the retried re-point", func() bool { p, _ := f.gw.Store.Prefix("kc-1"); return p == newPrefix })
	f.gw.repointMu.Lock()
	defer f.gw.repointMu.Unlock()
	conns.mu.Lock()
	defer conns.mu.Unlock()
	if len(conns.repoints) != 1 || conns.repoints[0][d.Ref] != Ref(newPrefix, d.Digest) {
		t.Fatalf("re-points %v", conns.repoints)
	}
	f.gw.retryMu.Lock()
	defer f.gw.retryMu.Unlock()
	if f.gw.retryTimer != nil || f.gw.retryWait != 0 {
		t.Fatal("retries go on after a pass that did everything")
	}
}
