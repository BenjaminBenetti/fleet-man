package imageserve_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// e2e_test.go drives the image server end to end: a real gateway
// (gateway.New + ServeListeners), fleetd's real tunnel client
// (remote.Manager, its registry connection metered by an Uplink) and the
// image server on the Manager's registry listener, with images delivered
// through Gateway.Deliver from a scripted `docker save`.

const testDaemonID = "3fa2c91e0b77d4e5a6b7c8d9e0f1a2b3"

// clusters is imageserve.Clusters in memory.
type clusters struct {
	mu sync.Mutex
	m  map[string]imageserve.Cluster
}

func newClusters(ids ...string) *clusters {
	c := &clusters{m: map[string]imageserve.Cluster{}}
	for _, id := range ids {
		c.m[id] = imageserve.Cluster{ID: id, PacePercent: 80}
	}
	return c
}

func (c *clusters) Lookup(id string) (imageserve.Cluster, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.m[id]
	return cl, ok
}

func (c *clusters) EnsureCredential(id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.m[id]
	if !ok {
		return "", fmt.Errorf("no connection %q", id)
	}
	if cl.Credential == "" {
		cl.Credential = fmt.Sprintf("cred-%s-%d", id, time.Now().UnixNano())
		c.m[id] = cl
	}
	return cl.Credential, nil
}

func (c *clusters) RotateCredential(id string) (string, error) {
	c.mu.Lock()
	cl := c.m[id]
	cl.Credential = ""
	c.m[id] = cl
	c.mu.Unlock()
	return c.EnsureCredential(id)
}

func (c *clusters) setPace(id string, pct int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.m[id]
	cl.PacePercent = pct
	c.m[id] = cl
}

// secrets records the pull Secrets applied.
type secrets struct {
	mu      sync.Mutex
	applied map[string]kubeimage.PullSecret
}

func (s *secrets) ApplyPullSecret(_ context.Context, p kubeimage.PullSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied == nil {
		s.applied = map[string]kubeimage.PullSecret{}
	}
	s.applied[p.Name] = p
	return nil
}

func (s *secrets) DeletePullSecret(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.applied, name)
	return nil
}

// login is the one Basic login of an applied gateway pull Secret.
func (s *secrets) login(t testing.TB, name string) (host, user, pass string) {
	t.Helper()
	s.mu.Lock()
	p, ok := s.applied[name]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("pull Secret %s was not applied", name)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password string } `json:"auths"`
	}
	if err := json.Unmarshal(p.DockerConfigJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	for h, a := range cfg.Auths {
		return h, a.Username, a.Password
	}
	t.Fatalf("pull Secret %s has no login", name)
	return
}

// startGateway serves a gateway on the given (or ephemeral loopback)
// listeners until the returned stop is called or the test ends.
func startGateway(t testing.TB, cfg gateway.Config, publicLn, grpcLn net.Listener) (publicAddr, grpcAddr string, stop func()) {
	t.Helper()
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	if publicLn == nil {
		if publicLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	if grpcLn == nil {
		if grpcLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = gw.ServeListeners(ctx, publicLn, grpcLn)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("the gateway did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return publicLn.Addr().String(), grpcLn.Addr().String(), stop
}

var registerStreamDesc = &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}

// registerDial opens the gateway's Register stream at addr, plain h2c when
// creds is nil.
func registerDial(addr string, creds credentials.TransportCredentials) func(context.Context, string) (net.Conn, error) {
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	return func(ctx context.Context, _ string) (net.Conn, error) {
		cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, err
		}
		stream, err := cc.NewStream(ctx, registerStreamDesc, tunnel.RegisterMethod, grpc.ForceCodec(tunnel.RawCodec{}))
		if err != nil {
			_ = cc.Close()
			return nil, err
		}
		return tunnel.NewStreamConn(stream, func() { _ = cc.Close() }), nil
	}
}

func tlsCreds(pool *x509.CertPool, serverName string) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12})
}

// fleetd is fleetd's image-delivery half: the tunnel client and the image
// server.
type fleetd struct {
	mgr      *remote.Manager
	images   *imageserve.Gateway
	uplink   *imageserve.Uplink
	clusters *clusters
	secrets  *secrets
	docker   imageserve.FakeDocker
	statuses chan *fleetgrpc.RemoteMcpStatus
	// attaches counts the registry connections, attachBytes what they sent.
	attaches    atomic.Int32
	attachBytes atomic.Int64
	stop        func()
}

type fleetdOptions struct {
	dial       func(context.Context, string) (net.Conn, error)
	gatewayURL string
	storeDir   string
	extra      []remote.Option
	// grpc turns on remote fleet control too; MCP and the registry are
	// always on.
	grpc bool
	// realDocker prepares images with the host's docker instead of the
	// scripted one.
	realDocker bool
	// clusters carries a previous fleetd's connections (and credentials).
	clusters *clusters
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.Conn.Write(p)
}

// startFleetd runs a tunnel client wanting the registry, with the image
// server on its registry listener.
func startFleetd(t testing.TB, o fleetdOptions) *fleetd {
	t.Helper()
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "mcp") }))
	t.Cleanup(mcp.Close)
	mcpPort, _ := strconv.Atoi(mcp.URL[strings.LastIndex(mcp.URL, ":")+1:])
	if o.storeDir == "" {
		o.storeDir = t.TempDir()
	}
	f := &fleetd{
		uplink:   &imageserve.Uplink{},
		clusters: o.clusters,
		secrets:  &secrets{},
		docker:   imageserve.NewFakeDocker(t),
		statuses: make(chan *fleetgrpc.RemoteMcpStatus, 256),
	}
	if f.clusters == nil {
		f.clusters = newClusters("kc-1", "kc-2")
	}
	docker := f.docker.Path()
	if o.realDocker {
		docker = ""
	}
	f.images = &imageserve.Gateway{Store: &imageserve.Store{Dir: o.storeDir, Docker: docker}, Clusters: f.clusters, Uplink: f.uplink}
	registryLis := remote.NewChanListener()
	opts := append([]remote.Option{
		remote.WithDialFunc(o.dial),
		remote.WithRegistryListener(registryLis),
		remote.WithRegistryChange(func(p string) { f.images.RegistryChanged(p) }),
		remote.WithRegistryConnWrapper(func(c net.Conn) net.Conn {
			f.attaches.Add(1)
			return f.uplink.Meter(countingConn{Conn: c, n: &f.attachBytes})
		}),
	}, o.extra...)
	f.mgr = remote.NewManager(mcpPort, "vtest", func(st *fleetgrpc.RemoteMcpStatus) {
		select {
		case f.statuses <- st:
		default:
		}
	}, opts...)
	f.images.Tunnel = f.mgr
	srv := &http.Server{Handler: f.images.Handler()}
	go func() { _ = srv.Serve(registryLis) }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.mgr.Run(ctx)
	}()
	f.mgr.Reconcile(true, o.grpc, false, true, o.gatewayURL)
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancel()
			<-done
			_ = srv.Close()
			_ = registryLis.Close()
		})
	}
	t.Cleanup(f.stop)
	return f
}

// waitPrefix waits for the gateway's registry prefix.
func (f *fleetd) waitPrefix(t testing.TB) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if p, _ := f.mgr.Registry(); p != "" {
			return p
		}
		if time.Now().After(deadline) {
			_, why := f.mgr.Registry()
			t.Fatalf("no registry prefix: %s", why)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// drainStatuses drops the statuses published so far.
func (f *fleetd) drainStatuses() {
	for {
		select {
		case <-f.statuses:
		default:
			return
		}
	}
}

// waitConnected waits for the tunnel to report CONNECTED.
func (f *fleetd) waitConnected(t testing.TB, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case st := <-f.statuses:
			if st.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				return
			}
		case <-deadline:
			t.Fatal("the tunnel never connected")
		}
	}
}

// target is connection id's delivery target.
func (f *fleetd) target(id string) kubeimage.Target {
	return kubeimage.Target{
		Cluster:  &fleet.KubeCluster{ID: id, Namespace: "dev", Image: fleet.KubeImageSettings{Mode: fleet.KubeImageGateway}},
		DaemonID: testDaemonID,
		Platform: "linux/amd64",
		Secrets:  f.secrets,
		Labels:   map[string]string{"fleet.dev/owner": "3fa2c91e0b77", "fleet.dev/cluster": id},
	}
}

// deliver delivers an archive's image to connection id and returns the
// delivery and the pull login.
func (f *fleetd) deliver(t testing.TB, id string, a imageserve.TestArchive) (kubeimage.GatewayDelivery, string, string) {
	t.Helper()
	f.docker.Add(t, a.ImageID, a.Archive)
	d, err := f.images.Deliver(context.Background(), f.target(id), kubeimage.Image{Ref: "vsc-test", Config: kubeshim.ImageConfig{ID: a.ImageID}, Built: true})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	_, user, pass := f.secrets.login(t, d.PullSecret)
	return d, user, pass
}

// get requests url, retrying while the gateway has not bound the session
// yet (its 404 for a rid it does not know).
func get(t testing.TB, method, url, user, pass string, header ...string) (*http.Response, []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "unknown or expired registry") && time.Now().Before(deadline) && !strings.Contains(url, "/v2/"+strings.Repeat("0", 64)) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return resp, body
	}
}

func TestEndToEndPullThroughTheGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	publicAddr, grpcAddr, _ := startGateway(t, gateway.Config{PublicURL: "https://gw.example.com", SessionKey: "e2e-key"}, nil, nil)
	f := startFleetd(t, fleetdOptions{dial: registerDial(grpcAddr, nil), gatewayURL: "https://gw.example.com"})
	prefix := f.waitPrefix(t)
	a := imageserve.NewTestArchive(t, "amd64", map[string]string{"etc/base": "base"}, map[string]string{"app": string(imageserve.RandomBytes(300<<10, 7))})
	d, user, pass := f.deliver(t, "kc-1", a)
	if d.Ref != prefix+"/devcontainer@"+a.Manifest {
		t.Fatalf("Ref %s, want %s/devcontainer@%s", d.Ref, prefix, a.Manifest)
	}
	rid := strings.TrimPrefix(prefix, "gw.example.com/")
	base := "http://" + publicAddr + "/v2/" + rid + "/devcontainer/"

	// Manifest: HEAD and GET carry its media type, length and digest.
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, body := get(t, method, base+"manifests/"+a.Manifest, user, pass)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" ||
			resp.Header.Get("Docker-Content-Digest") != a.Manifest || resp.Header.Get("Content-Length") != strconv.Itoa(len(a.ManifestBytes)) {
			t.Fatalf("%s manifest through the gateway: %d %v", method, resp.StatusCode, resp.Header)
		}
		if method == http.MethodGet && !bytes.Equal(body, a.ManifestBytes) {
			t.Fatal("manifest body differs")
		}
	}
	// Blobs, whole and resumed.
	for _, layer := range a.Layers {
		ld := imageserve.Sha256Digest(layer)
		resp, body := get(t, http.MethodGet, base+"blobs/"+ld, user, pass)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, layer) || resp.Header.Get("Docker-Content-Digest") != ld {
			t.Fatalf("blob through the gateway: %d", resp.StatusCode)
		}
		resp, body = get(t, http.MethodGet, base+"blobs/"+ld, user, pass, "Range", "bytes=10-")
		if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, layer[10:]) {
			t.Fatalf("resumed blob through the gateway: %d", resp.StatusCode)
		}
	}
	if f.attaches.Load() == 0 || f.attachBytes.Load() < int64(len(a.Layers[1])) {
		t.Fatalf("the pull did not use the registry connection (%d connections, %d bytes)", f.attaches.Load(), f.attachBytes.Load())
	}

	// Without the credential: fleetd's Basic challenge, through the gateway.
	resp, _ := get(t, http.MethodHead, base+"manifests/"+a.Manifest, "", "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != `Basic realm="fleet"` {
		t.Fatalf("no credential: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// Another connection's credential sees nothing.
	cred2, _ := f.clusters.EnsureCredential("kc-2")
	if resp, _ := get(t, http.MethodHead, base+"manifests/"+a.Manifest, "kc-2", cred2); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another connection's credential: %d", resp.StatusCode)
	}
	// An unknown registry id: the gateway's 404.
	resp, body := get(t, http.MethodGet, "http://"+publicAddr+"/v2/"+strings.Repeat("0", 64)+"/devcontainer/manifests/"+a.Manifest, user, pass)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "NAME_UNKNOWN") {
		t.Fatalf("unknown rid: %d %s", resp.StatusCode, body)
	}

	// The daemon turns the registry off: its session no longer has the
	// feature, and its registry id 404s like an unknown one.
	f.mgr.Reconcile(true, false, false, false, "https://gw.example.com")
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, body = get(t, http.MethodHead, base+"manifests/"+a.Manifest, user, pass)
		if resp.StatusCode == http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a session without the feature: %d %s", resp.StatusCode, body)
	}
	if p, why := f.mgr.Registry(); p != "" || why == "" {
		t.Fatalf("Registry after turning it off: %q %q", p, why)
	}
}
