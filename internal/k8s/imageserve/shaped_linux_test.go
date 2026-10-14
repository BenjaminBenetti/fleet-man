//go:build linux

package imageserve_test

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// shaped_linux_test.go is the throughput spike for gateway delivery: a
// multi-GB pull through a real gateway over the dedicated registry
// connection, on a daemon uplink shaped to 10 Mbit/s with a 200 ms queue
// (tc tbf), while a remote-gRPC call runs on the main tunnel every 250 ms.
// Neither connection's keepalive may trip, and the gRPC calls' p99 latency
// must stay bounded.
//
// The gateway lives in its own network namespace behind a veth pair whose
// daemon-side egress is shaped. It needs root (netns, tc), so it runs in the
// privileged QA container:
//
//	go test -c -o imageserve.test ./internal/k8s/imageserve/
//	sudo FLEET_IMAGESERVE_SHAPED=1 ./imageserve.test -test.run Shaped -test.v -test.timeout 2h
//
// FLEET_IMAGESERVE_SHAPED_MB sets the image size (default 2048) and
// FLEET_IMAGESERVE_SHAPED_PACE the upload pace in percent (default 80; 0
// is unpaced, for comparison).

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// inNetns runs fn on an OS thread inside the network namespace at nsPath:
// sockets keep the namespace they were created in.
func inNetns(nsPath string, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		self, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer self.Close()
		target, err := os.Open(nsPath)
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("setns: %w", err)
			return
		}
		ferr := fn()
		if err := unix.Setns(int(self.Fd()), unix.CLONE_NEWNET); err != nil {
			// The thread stays locked, so the runtime retires it.
			errc <- fmt.Errorf("setns back: %w", err)
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}

// listenIn opens TCP listeners on addrs inside a network namespace.
func listenIn(t *testing.T, nsPath string, addrs ...string) []net.Listener {
	t.Helper()
	var lns []net.Listener
	if err := inNetns(nsPath, func() error {
		for _, a := range addrs {
			ln, err := net.Listen("tcp", a)
			if err != nil {
				return err
			}
			lns = append(lns, ln)
		}
		return nil
	}); err != nil {
		t.Fatalf("listen in the gateway namespace: %v", err)
	}
	return lns
}

// dialIn dials from inside a network namespace: the node and the remote
// client sit beside the gateway, off the daemon's shaped uplink.
func dialIn(nsPath string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var c net.Conn
		err := inNetns(nsPath, func() error {
			var err error
			c, err = (&net.Dialer{}).DialContext(ctx, network, addr)
			return err
		})
		return c, err
	}
}

// layerStream is a pseudo-random layer: the same bytes each time it is
// read from its seed.
type layerStream struct {
	size int64
	seed uint64
}

func (l layerStream) reader() io.Reader {
	return io.LimitReader(rand.NewChaCha8([32]byte{byte(l.seed), byte(l.seed >> 8), 0xfe}), l.size)
}

func (l layerStream) digest() string {
	h := sha256.New()
	_, _ = io.Copy(h, l.reader())
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// spikeImage is a single-platform image whose layers are big pseudo-random
// blobs (labelled gzip; nothing unpacks them), and its small documents.
type spikeImage struct {
	layers                    []layerStream
	digests                   []string
	config, manifest, ix, top []byte
	manifestDigest, imageID   string
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newSpikeImage(layers []layerStream) spikeImage {
	img := spikeImage{layers: layers}
	var descs []map[string]any
	for _, l := range layers {
		d := l.digest()
		img.digests = append(img.digests, d)
		descs = append(descs, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": d, "size": l.size})
	}
	img.config, _ = json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": img.digests}})
	img.manifest, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": sha(img.config), "size": len(img.config)}, "layers": descs})
	img.manifestDigest = sha(img.manifest)
	img.ix, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": img.manifestDigest, "size": len(img.manifest), "platform": map[string]string{"os": "linux", "architecture": "amd64"}}}})
	img.imageID = sha(img.ix)
	img.top, _ = json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": img.imageID, "size": len(img.ix)}}})
	return img
}

// write streams the image as a containerd-store `docker save` archive,
// without holding the layers in memory.
func (img spikeImage) write(w io.Writer) error {
	tw := tar.NewWriter(w)
	put := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	for i, l := range img.layers {
		if err := put("blobs/sha256/"+strings.TrimPrefix(img.digests[i], "sha256:"), l.size, l.reader()); err != nil {
			return err
		}
	}
	for _, b := range [][]byte{img.config, img.manifest, img.ix} {
		if err := put("blobs/sha256/"+strings.TrimPrefix(sha(b), "sha256:"), int64(len(b)), strings.NewReader(string(b))); err != nil {
			return err
		}
	}
	if err := put("index.json", int64(len(img.top)), strings.NewReader(string(img.top))); err != nil {
		return err
	}
	return tw.Close()
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func TestShapedUplinkPull(t *testing.T) {
	if os.Getenv("FLEET_IMAGESERVE_SHAPED") == "" {
		t.Skip("set FLEET_IMAGESERVE_SHAPED=1 (as root) to run the shaped-uplink throughput spike")
	}
	if os.Geteuid() != 0 {
		t.Fatal("the shaped-uplink spike needs root (network namespaces, tc)")
	}
	sizeMB, pace := envInt("FLEET_IMAGESERVE_SHAPED_MB", 2048), envInt("FLEET_IMAGESERVE_SHAPED_PACE", 80)
	t.Setenv("HOME", t.TempDir())

	// The gateway's namespace, a veth pair, and the daemon's shaped uplink.
	tag := strconv.FormatInt(time.Now().UnixNano()%1e6, 16)
	ns, vd, vg := "fleet-shaped-"+tag, "vd"+tag, "vg"+tag
	run(t, "ip", "netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run(t, "ip", "link", "add", vd, "type", "veth", "peer", "name", vg)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", vd).Run() })
	run(t, "ip", "link", "set", vg, "netns", ns)
	run(t, "ip", "addr", "add", "10.77.0.1/24", "dev", vd)
	run(t, "ip", "link", "set", vd, "up")
	run(t, "ip", "netns", "exec", ns, "ip", "addr", "add", "10.77.0.2/24", "dev", vg)
	run(t, "ip", "netns", "exec", ns, "ip", "link", "set", vg, "up")
	run(t, "ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up")
	run(t, "tc", "qdisc", "add", "dev", vd, "root", "tbf", "rate", "10mbit", "burst", "32kbit", "latency", "200ms")
	out, _ := exec.Command("tc", "qdisc", "show", "dev", vd).CombinedOutput()
	t.Logf("daemon uplink: %s", strings.TrimSpace(string(out)))

	lns := listenIn(t, "/run/netns/"+ns, "10.77.0.2:0", "10.77.0.2:0")
	publicAddr, grpcAddr, _ := startGateway(t, gateway.Config{PublicURL: "https://gw.example.com", SessionKey: "shaped"}, lns[0], lns[1])

	// fleetd: remote fleet (gRPC) and the registry on the main tunnel.
	grpcLis := remote.NewChanListener()
	grpcSrv := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, health.NewServer())
	go func() { _ = grpcSrv.Serve(grpcLis) }()
	defer grpcSrv.Stop()
	f := startFleetd(t, fleetdOptions{dial: registerDial(grpcAddr, nil), gatewayURL: "https://gw.example.com", grpc: true, extra: []remote.Option{remote.WithGRPCListener(grpcLis)}})
	var publicID string
	for publicID == "" {
		select {
		case st := <-f.statuses:
			if st.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				publicID = st.GetPublicUrl()[strings.LastIndex(st.GetPublicUrl(), "/")+1:]
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the tunnel never connected")
		}
	}
	prefix := f.waitPrefix(t)
	rid := strings.TrimPrefix(prefix, "gw.example.com/")

	// The image: four pseudo-random layers.
	var layers []layerStream
	for i := range 4 {
		layers = append(layers, layerStream{size: int64(sizeMB) << 20 / 4, seed: uint64(i + 1)})
	}
	img := newSpikeImage(layers)
	digests := img.digests
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(img.write(pw)) }()
	prep, err := f.images.Store.Import(context.Background(), pr, img.imageID, "linux/amd64")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if prep.Digest != img.manifestDigest {
		t.Fatalf("imported %s, want %s", prep.Digest, img.manifestDigest)
	}
	f.clusters.setPace("kc-1", pace)
	if err := f.images.Store.Ref("kc-1", prep.Digest, "spike"); err != nil {
		t.Fatal(err)
	}
	cred, _ := f.clusters.EnsureCredential("kc-1")
	f.mgr.WarmRegistry()

	// The remote-gRPC probe on the main tunnel.
	nsDial := dialIn("/run/netns/" + ns)
	cc, err := grpc.NewClient("passthrough:///"+grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return nsDial(ctx, "tcp", addr) }))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	hc := healthpb.NewHealthClient(cc)
	probeCtx, stopProbe := context.WithCancel(context.Background())
	var lat []time.Duration
	var probeErrs int
	var probeMu sync.Mutex
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-probeCtx.Done():
				return
			case <-tick.C:
			}
			ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(probeCtx, "fleet-session", publicID), 30*time.Second)
			start := time.Now()
			_, err := hc.Check(ctx, &healthpb.HealthCheckRequest{})
			cancel()
			probeMu.Lock()
			if err != nil && probeCtx.Err() == nil {
				probeErrs++
			} else if err == nil {
				lat = append(lat, time.Since(start))
			}
			probeMu.Unlock()
		}
	}()

	// The pull: the manifest, then the layers three at a time, as containerd
	// fetches them.
	f.drainStatuses()
	base := "http://" + publicAddr + "/v2/" + rid + "/devcontainer/"
	node := &http.Client{Transport: &http.Transport{DialContext: nsDial, MaxIdleConnsPerHost: 4}}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest(http.MethodGet, base+"manifests/"+prep.Digest, nil)
		req.SetBasicAuth("kc-1", cred)
		resp, err := node.Do(req)
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode != http.StatusNotFound || attempt == 100 {
			t.Fatalf("manifest: %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond) // the gateway binds the session just after its reply
	}
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var pullErr error
	var errMu sync.Mutex
	for i, d := range digests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, _ := http.NewRequest(http.MethodGet, base+"blobs/"+d, nil)
			req.SetBasicAuth("kc-1", cred)
			resp, err := node.Do(req)
			if err == nil {
				h := sha256.New()
				var n int64
				n, err = io.Copy(h, resp.Body)
				resp.Body.Close()
				if err == nil && ("sha256:"+hex.EncodeToString(h.Sum(nil)) != d || n != layers[i].size) {
					err = fmt.Errorf("layer %d: %d bytes, wrong digest", i, n)
				}
			}
			if err != nil {
				errMu.Lock()
				pullErr = err
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	took := time.Since(start)
	stopProbe()
	<-probeDone
	if pullErr != nil {
		t.Fatalf("pull: %v", pullErr)
	}

	// Neither keepalive tripped: no reconnect of the main tunnel, one
	// registry connection for the whole pull.
	reconnects := 0
	for {
		select {
		case st := <-f.statuses:
			if st.GetState() != fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				reconnects++
			}
			continue
		default:
		}
		break
	}
	total := int64(sizeMB) << 20
	probeMu.Lock()
	defer probeMu.Unlock()
	rate := float64(total) / took.Seconds()
	t.Logf("pulled %d MiB in %v: %.2f Mbit/s (%.0f%% of 10 Mbit/s), pace %d%%, uplink measured %.2f Mbit/s",
		sizeMB, took.Round(time.Second), rate*8/1e6, rate*8/1e7*100, pace, f.uplink.Rate()*8/1e6)
	t.Logf("remote gRPC on the main tunnel: %d calls, p50 %v, p99 %v, max %v, %d errors",
		len(lat), percentile(lat, 0.5).Round(time.Millisecond), percentile(lat, 0.99).Round(time.Millisecond), percentile(lat, 1).Round(time.Millisecond), probeErrs)
	t.Logf("registry connections: %d; main tunnel reconnects: %d", f.attaches.Load(), reconnects)
	if reconnects != 0 {
		t.Errorf("the main tunnel reconnected %d times during the pull (keepalive tripped)", reconnects)
	}
	if f.attaches.Load() != 1 {
		t.Errorf("%d registry connections during the pull, want 1 (its keepalive tripped)", f.attaches.Load())
	}
	if probeErrs != 0 {
		t.Errorf("%d remote gRPC calls failed during the pull", probeErrs)
	}
	if p99 := percentile(lat, 0.99); p99 > 2*time.Second {
		t.Errorf("remote gRPC p99 %v during the pull, want at most 2s", p99)
	}
}
