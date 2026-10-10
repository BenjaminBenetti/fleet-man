package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/credentials/insecure"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// registry_route_test.go is the gateway end-to-end test of gateway image
// delivery: a fleetd stand-in registers with FeatureRegistry (and optionally
// attaches a registry connection) over the real Register transport, and HTTPS
// clients pull through the public /v2/ route.

const (
	testRegistryUser = "fleet"
	testRegistryPass = "pull-secret"
)

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// registryPull is one request against the gateway's public listener.
func registryPull(t *testing.T, client *http.Client, method, url, auth, rangeHdr string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// registryIDOf returns the rid of a PublicRegistry ("<host>/<rid>").
func registryIDOf(t *testing.T, publicRegistry, host string) string {
	t.Helper()
	rid, ok := strings.CutPrefix(publicRegistry, host+"/")
	if !ok || !isRegistryID(rid) {
		t.Fatalf("PublicRegistry %q is not %s/<rid>", publicRegistry, host)
	}
	return rid
}

// registryStack is a registry-serving gateway with one registered fleetd
// stand-in that serves MCP and the test registry on the main tunnel.
type registryStack struct {
	gw         *Server
	publicAddr string
	grpcAddr   string
	pool       *x509.CertPool
	client     *http.Client
	reg        *testRegistry
	main       *fakeDaemon
	rid        string
}

func newRegistryStack(t *testing.T, maxSessions int) *registryStack {
	t.Helper()
	cert, pool := genTestTLS(t)
	gw, publicAddr, grpcAddr := startRegistryGateway(t, cert, "registry-key", maxSessions)
	st := &registryStack{
		gw: gw, publicAddr: publicAddr, grpcAddr: grpcAddr, pool: pool,
		client: httpsClient(pool),
		reg: &testRegistry{
			img:  newTestImage(t, "amd64", []string{"/hello"}, testFile{name: "hello", mode: 0o755, body: []byte("#!/bin/sh\n")}),
			user: testRegistryUser, pass: testRegistryPass,
		},
	}
	st.main = connectFake(t, grpcAddr, tlsCredsFor(pool), tunnel.RegisterRequest{Features: []string{tunnel.FeatureRegistry}},
		map[byte]http.Handler{tunnel.TagMCP: mcpRoot, tunnel.TagRegistry: st.reg.handler("main")}, nil)
	if st.main.reply.Error != "" {
		t.Fatalf("register: %s", st.main.reply.Error)
	}
	st.rid = registryIDOf(t, st.main.reply.PublicRegistry, "gw.example.com")
	waitRegistered(t, gw, publicIDOf(t, st.main.reply.PublicURL))
	return st
}

func (st *registryStack) url(rid, rest string) string {
	return "https://" + st.publicAddr + "/v2/" + rid + "/" + rest
}

func (st *registryStack) manifestPath() string {
	return "devcontainer/manifests/" + st.reg.img.manifestDigest
}

// attach opens a registry connection for the stack's session, serving the
// test registry as via.
func (st *registryStack) attach(t *testing.T, via string) *fakeDaemon {
	t.Helper()
	d := connectFake(t, st.grpcAddr, tlsCredsFor(st.pool),
		tunnel.RegisterRequest{Attach: tunnel.AttachRegistry, SessionID: st.main.reply.SessionID},
		map[byte]http.Handler{tunnel.TagRegistry: st.reg.handler(via)}, nil)
	if d.reply.Error != "" {
		t.Fatalf("attach refused: %s", d.reply.Error)
	}
	waitFor(t, "the registry connection to bind", func() bool {
		sess := st.gw.reg.lookupRegistry(st.rid)
		return sess != nil && sess.currentRegistryYamux() != nil
	})
	return d
}

// waitFor polls cond for up to 5s.
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

// TestGatewayRegistryRoute pulls through the gateway end to end: the base
// challenge, manifest HEAD and GET with the headers containerd needs, a ranged
// blob GET, the daemon's own 401, the method restriction and the 404s.
func TestGatewayRegistryRoute(t *testing.T) {
	st := newRegistryStack(t, 64)
	reply := st.main.reply
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureRegistry) {
		t.Fatalf("registry not negotiated: %v", reply.Features)
	}
	publicID := publicIDOf(t, reply.PublicURL)
	if st.rid == publicID {
		t.Fatal("the registry id must not be the public id")
	}
	auth := basicAuth(testRegistryUser, testRegistryPass)

	// Bare /v2/: the gateway's own Basic challenge, which docker and CRI-O
	// probe for before pulling.
	resp, _ := registryPull(t, st.client, http.MethodGet, "https://"+st.publicAddr+"/v2/", "", "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != `Basic realm="fleet"` {
		t.Fatalf("/v2/ -> %d %q, want 401 with a Basic challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("/v2/ must name the distribution API version, got %q", resp.Header.Get("Docker-Distribution-API-Version"))
	}

	// Manifest HEAD: size, digest and media type reach the puller, no body.
	img := st.reg.img
	resp, body := registryPull(t, st.client, http.MethodHead, st.url(st.rid, st.manifestPath()), auth, "")
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("manifest HEAD -> %d with %d body bytes", resp.StatusCode, len(body))
	}
	if resp.ContentLength != int64(len(img.manifest)) ||
		resp.Header.Get("Docker-Content-Digest") != img.manifestDigest ||
		resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("manifest HEAD headers: length %d digest %q type %q", resp.ContentLength,
			resp.Header.Get("Docker-Content-Digest"), resp.Header.Get("Content-Type"))
	}

	// Manifest GET.
	resp, body = registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), auth, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, img.manifest) {
		t.Fatalf("manifest GET -> %d, body match %v", resp.StatusCode, bytes.Equal(body, img.manifest))
	}

	// A ranged blob GET resumes mid-layer.
	var layerDigest string
	var layer []byte
	for d, b := range img.blobs {
		if len(b) > len(layer) {
			layerDigest, layer = d, b
		}
	}
	resp, body = registryPull(t, st.client, http.MethodGet, st.url(st.rid, "devcontainer/blobs/"+layerDigest), auth, "bytes=5-")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, layer[5:]) {
		t.Fatalf("ranged blob GET -> %d, body match %v", resp.StatusCode, bytes.Equal(body, layer[5:]))
	}

	// Without the credential the daemon's own challenge comes back untouched.
	resp, _ = registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), "", "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != `Basic realm="fleet"` {
		t.Fatalf("unauthenticated manifest GET -> %d %q, want the daemon's 401", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// The daemon saw its own paths (rid stripped), the credential and the range.
	hits := st.reg.seen()
	if len(hits) != 4 {
		t.Fatalf("daemon saw %d requests, want 4: %+v", len(hits), hits)
	}
	want := []registryHit{
		{via: "main", method: http.MethodHead, path: "/v2/" + st.manifestPath(), auth: auth},
		{via: "main", method: http.MethodGet, path: "/v2/" + st.manifestPath(), auth: auth},
		{via: "main", method: http.MethodGet, path: "/v2/devcontainer/blobs/" + layerDigest, auth: auth, rangeHdr: "bytes=5-"},
		{via: "main", method: http.MethodGet, path: "/v2/" + st.manifestPath()},
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("request %d: got %+v, want %+v", i, hits[i], want[i])
		}
	}

	// Pull-only: other methods stop at the gateway.
	resp, _ = registryPull(t, st.client, http.MethodPut, st.url(st.rid, st.manifestPath()), auth, "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT -> %d, want 405", resp.StatusCode)
	}
	if n := len(st.reg.seen()); n != 4 {
		t.Fatalf("a PUT reached the daemon (%d requests)", n)
	}

	// Unknown rids 404, the public id included (it is not a registry id).
	for name, rid := range map[string]string{"unknown": hexID(7), "public id": publicID, "secret": reply.SessionID} {
		resp, body := registryPull(t, st.client, http.MethodGet, st.url(rid, st.manifestPath()), auth, "")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "NAME_UNKNOWN") {
			t.Fatalf("%s rid -> %d %s, want 404 NAME_UNKNOWN", name, resp.StatusCode, body)
		}
	}

	// The registry feature alone tags the wire, and MCP still works on it.
	if body := getBody(t, st.client, "https://"+st.publicAddr+"/mcp/"+publicID, ""); body != "root" {
		t.Fatalf("MCP on a registry-tagged tunnel -> %q", body)
	}
}

// TestGatewayRegistryNeedsTheFeature: a session that did not negotiate the
// registry gets no PublicRegistry and 404s on its registry id (which its token
// still carries), and keeps the legacy untagged wire. A gateway that does not
// offer the registry negotiates it for nobody and 404s the API root.
func TestGatewayRegistryNeedsTheFeature(t *testing.T) {
	cert, pool := genTestTLS(t)
	gw, publicAddr, grpcAddr := startRegistryGateway(t, cert, "k", 64)
	client := httpsClient(pool)

	legacy := connectFake(t, grpcAddr, tlsCredsFor(pool), tunnel.RegisterRequest{}, nil, mcpRoot)
	if legacy.reply.PublicRegistry != "" || len(legacy.reply.Features) != 0 {
		t.Fatalf("a session without the feature got %q / %v", legacy.reply.PublicRegistry, legacy.reply.Features)
	}
	claims, ok := gw.reg.signer.verify(legacy.reply.SessionToken)
	if !ok || claims.RegistryID == "" {
		t.Fatal("every session's token carries its registry id")
	}
	publicID := publicIDOf(t, legacy.reply.PublicURL)
	waitRegistered(t, gw, publicID)
	resp, _ := registryPull(t, client, http.MethodGet, "https://"+publicAddr+"/v2/"+claims.RegistryID+"/devcontainer/manifests/sha256:x", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("registry route of a session without the feature -> %d, want 404", resp.StatusCode)
	}
	if body := getBody(t, client, "https://"+publicAddr+"/mcp/"+publicID, ""); body != "root" {
		t.Fatalf("legacy MCP -> %q", body)
	}

	// A gateway that does not offer the registry (no session key, or an
	// unusable public URL).
	_, plainPublic, plainGRPC := startTestGateway(t, cert, "https://gw.example.com")
	d := connectFake(t, plainGRPC, tlsCredsFor(pool), tunnel.RegisterRequest{Features: []string{tunnel.FeatureRegistry}}, nil, mcpRoot)
	if d.reply.PublicRegistry != "" || tunnel.HasFeature(d.reply.Features, tunnel.FeatureRegistry) {
		t.Fatalf("a gateway without the registry negotiated it: %q %v", d.reply.PublicRegistry, d.reply.Features)
	}
	resp, _ = registryPull(t, client, http.MethodGet, "https://"+plainPublic+"/v2/", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v2/ on a gateway without the registry -> %d, want 404", resp.StatusCode)
	}
}

// TestGatewayRegistryAttach covers the auxiliary registry connection: registry
// streams move onto it while it is bound (MCP stays on the main tunnel), a
// newer attach supersedes an older one, and when it goes the route falls back
// to the main tunnel. It does not count against MaxSessions, and attaches
// naming no registry session are refused.
func TestGatewayRegistryAttach(t *testing.T) {
	st := newRegistryStack(t, 1) // MaxSessions 1: the main tunnel fills it
	auth := basicAuth(testRegistryUser, testRegistryPass)
	viaOf := func() string {
		hits := st.reg.seen()
		return hits[len(hits)-1].via
	}
	pull := func() {
		t.Helper()
		resp, _ := registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), auth, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pull -> %d", resp.StatusCode)
		}
	}

	pull()
	if via := viaOf(); via != "main" {
		t.Fatalf("with no registry connection the pull went via %q, want main", via)
	}

	first := st.attach(t, "attach-1")
	if !tunnel.HasFeature(first.reply.Features, tunnel.FeatureRegistry) || first.reply.SessionToken != "" || first.reply.PublicURL != "" {
		t.Fatalf("attach reply must confirm the role and mint nothing: %+v", first.reply)
	}
	pull()
	if via := viaOf(); via != "attach-1" {
		t.Fatalf("pull went via %q, want the registry connection", via)
	}
	if body := getBody(t, st.client, "https://"+st.publicAddr+"/mcp/"+publicIDOf(t, st.main.reply.PublicURL), ""); body != "root" {
		t.Fatalf("MCP must stay on the main tunnel, got %q", body)
	}

	// One per session: a second attach supersedes the first.
	sess := st.gw.reg.lookupRegistry(st.rid)
	firstYm := sess.currentRegistryYamux()
	second := st.attach(t, "attach-2")
	waitFor(t, "the superseded registry connection to close", firstYm.IsClosed)
	pull()
	if via := viaOf(); via != "attach-2" {
		t.Fatalf("pull went via %q, want the newer registry connection", via)
	}

	// The registry connection goes: pulls fall back to the main tunnel.
	second.close()
	waitFor(t, "the registry slot to clear", func() bool { return sess.currentRegistryYamux() == nil })
	pull()
	if via := viaOf(); via != "main" {
		t.Fatalf("after the registry connection closed the pull went via %q, want main", via)
	}

	// The cap is full (MaxSessions 1), yet attaching still works; a new
	// registration does not.
	if third := st.attach(t, "attach-3"); third.reply.Error != "" {
		t.Fatalf("attach at capacity refused: %s", third.reply.Error)
	}
	if d := connectFake(t, st.grpcAddr, tlsCredsFor(st.pool), tunnel.RegisterRequest{}, nil, mcpRoot); !strings.Contains(d.reply.Error, errAtCapacity.Error()) {
		t.Fatalf("a second registration must hit the cap, got %q", d.reply.Error)
	}

	// Refusals: an unknown secret, the public or registry id, an unknown role.
	for name, req := range map[string]tunnel.RegisterRequest{
		"unknown secret": {Attach: tunnel.AttachRegistry, SessionID: hexID(5)},
		"public id":      {Attach: tunnel.AttachRegistry, SessionID: publicIDOf(t, st.main.reply.PublicURL)},
		"registry id":    {Attach: tunnel.AttachRegistry, SessionID: st.rid},
		"unknown role":   {Attach: "bulk", SessionID: st.main.reply.SessionID},
	} {
		if d := connectFake(t, st.grpcAddr, tlsCredsFor(st.pool), req, nil, nil); d.reply.Error == "" {
			t.Errorf("%s: attach must be refused", name)
		}
	}
}

// TestGatewayRegistryAttachNeedsTheFeature: a session that did not negotiate
// the registry cannot attach a registry connection, and a re-registration that
// drops the feature closes one that is bound.
func TestGatewayRegistryAttachNeedsTheFeature(t *testing.T) {
	cert, pool := genTestTLS(t)
	gw, _, grpcAddr := startRegistryGateway(t, cert, "k", 64)
	plain := connectFake(t, grpcAddr, tlsCredsFor(pool), tunnel.RegisterRequest{}, nil, mcpRoot)
	waitRegistered(t, gw, publicIDOf(t, plain.reply.PublicURL))
	if d := connectFake(t, grpcAddr, tlsCredsFor(pool), tunnel.RegisterRequest{Attach: tunnel.AttachRegistry, SessionID: plain.reply.SessionID}, nil, nil); d.reply.Error == "" {
		t.Fatal("a session without the registry feature must not attach")
	}

	st := newRegistryStack(t, 64)
	sess := st.gw.reg.lookupRegistry(st.rid)
	attached := st.attach(t, "attach")
	_ = attached
	ym := sess.currentRegistryYamux()
	again := connectFake(t, st.grpcAddr, tlsCredsFor(st.pool), tunnel.RegisterRequest{SessionID: st.main.reply.SessionID}, nil, mcpRoot)
	if again.reply.PublicRegistry != "" {
		t.Fatal("a re-registration without the feature must get no PublicRegistry")
	}
	waitFor(t, "the registry connection to close", ym.IsClosed)
	resp, _ := registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("registry route after the feature was dropped -> %d, want 404", resp.StatusCode)
	}
}

// TestGatewayRegistryStreamCap: at most maxRegistryStreams registry requests
// of one session are in flight at once; the rest wait for a slot and then
// complete.
func TestGatewayRegistryStreamCap(t *testing.T) {
	st := newRegistryStack(t, 64)
	st.attach(t, "attach")
	st.reg.hold = make(chan struct{})
	auth := basicAuth(testRegistryUser, testRegistryPass)
	var layerDigest string
	for d := range st.reg.img.blobs {
		layerDigest = d
	}

	const pulls = maxRegistryStreams + 4
	codes := make(chan int, pulls)
	var wg sync.WaitGroup
	for range pulls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, st.url(st.rid, "devcontainer/blobs/"+layerDigest), nil)
			req.Header.Set("Authorization", auth)
			resp, err := st.client.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	inFlight := func() int {
		st.reg.mu.Lock()
		defer st.reg.mu.Unlock()
		return st.reg.inFlight
	}
	waitFor(t, "the cap to fill", func() bool { return inFlight() >= maxRegistryStreams })
	time.Sleep(200 * time.Millisecond) // give an over-cap request time to slip through
	if n := inFlight(); n != maxRegistryStreams {
		t.Fatalf("%d registry requests in flight, want the cap of %d", n, maxRegistryStreams)
	}
	close(st.reg.hold)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a capped pull ended %d, want 200", code)
		}
	}
	if st.reg.peak != maxRegistryStreams {
		t.Fatalf("peak concurrency %d, want %d", st.reg.peak, maxRegistryStreams)
	}
}

// TestGatewayRegistryLargeBlobOverAttach pulls a 24 MiB layer through the
// registry connection. The daemon writes it in one call, so yamux data frames
// grow to the 4 MiB stream window and StreamConn must keep each gRPC message
// within the receive limit; a corrupted reassembly would show here too.
func TestGatewayRegistryLargeBlobOverAttach(t *testing.T) {
	st := newRegistryStack(t, 64)
	st.reg.singleWrite = true
	big := make([]byte, 24<<20)
	for i := range big {
		big[i] = byte(i*7 + i>>12)
	}
	digest := digestOf(big)
	st.reg.img.blobs[digest] = big
	st.attach(t, "attach")

	req, _ := http.NewRequest(http.MethodGet, st.url(st.rid, "devcontainer/blobs/"+digest), nil)
	req.Header.Set("Authorization", basicAuth(testRegistryUser, testRegistryPass))
	resp, err := st.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || n != int64(len(big)) {
		t.Fatalf("large blob -> %d, %d bytes, err %v", resp.StatusCode, n, err)
	}
	if got := "sha256:" + hexOf(h.Sum(nil)); got != digest {
		t.Fatalf("large blob digest %s, want %s", got, digest)
	}
	if via := st.reg.seen()[0].via; via != "attach" {
		t.Fatalf("large blob went via %q, want the registry connection", via)
	}
}

// TestGatewayRegistryRestartKeepsReference: a restarted gateway with the same
// --session-key gives the daemon the same PublicRegistry, so image references
// already in the cluster keep pulling.
func TestGatewayRegistryRestartKeepsReference(t *testing.T) {
	cert, pool := genTestTLS(t)
	_, _, grpc1 := startRegistryGateway(t, cert, "restart-key", 64)
	first := connectFake(t, grpc1, tlsCredsFor(pool), tunnel.RegisterRequest{Features: []string{tunnel.FeatureRegistry}}, map[byte]http.Handler{}, nil)
	first.close()

	gw2, public2, grpc2 := startRegistryGateway(t, cert, "restart-key", 64)
	reg := &testRegistry{img: newTestImage(t, "amd64", nil), user: testRegistryUser, pass: testRegistryPass}
	second := connectFake(t, grpc2, tlsCredsFor(pool), tunnel.RegisterRequest{
		SessionID: first.reply.SessionID, SessionToken: first.reply.SessionToken,
		Features: []string{tunnel.FeatureRegistry},
	}, map[byte]http.Handler{tunnel.TagRegistry: reg.handler("main")}, nil)
	if second.reply.PublicRegistry != first.reply.PublicRegistry {
		t.Fatalf("PublicRegistry changed across the restart: %q -> %q", first.reply.PublicRegistry, second.reply.PublicRegistry)
	}
	waitRegistered(t, gw2, publicIDOf(t, second.reply.PublicURL))
	rid := registryIDOf(t, second.reply.PublicRegistry, "gw.example.com")
	resp, _ := registryPull(t, httpsClient(pool), http.MethodGet,
		"https://"+public2+"/v2/"+rid+"/devcontainer/manifests/"+reg.img.manifestDigest,
		basicAuth(testRegistryUser, testRegistryPass), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull through the restarted gateway -> %d", resp.StatusCode)
	}
}

// TestGatewayRegistryDaemonOffline: while the daemon is away (its session
// reserved for sessionTTL) a pull gets 502; once the session is reaped, 404.
// fleetd classifies both as "daemon was offline".
func TestGatewayRegistryDaemonOffline(t *testing.T) {
	st := newRegistryStack(t, 64)
	st.attach(t, "attach").close()
	st.main.close()
	sess := st.gw.reg.lookupRegistry(st.rid)
	waitFor(t, "the tunnel to close", func() bool { return sess.currentYamux().IsClosed() })

	resp, body := registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), "", "")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "UNAVAILABLE") {
		t.Fatalf("pull while the daemon is away -> %d %s, want 502", resp.StatusCode, body)
	}

	now := time.Now()
	st.gw.reg.reap(now, sessionTTL)
	st.gw.reg.reap(now.Add(2*sessionTTL), sessionTTL)
	resp, _ = registryPull(t, st.client, http.MethodGet, st.url(st.rid, st.manifestPath()), "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pull after the session was reaped -> %d, want 404", resp.StatusCode)
	}
}

// TestRegistryHostFor is the gate on offering FeatureRegistry: a session key,
// an https public URL, and no path prefix.
func TestRegistryHostFor(t *testing.T) {
	cases := []struct {
		name, publicURL, key string
		want                 string
	}{
		{"https host", "https://gw.example.com", "k", "gw.example.com"},
		{"https host and port", "https://gw.example.com:8443", "k", "gw.example.com:8443"},
		{"host lowercased", "https://GW.Example.com", "k", "gw.example.com"},
		{"no session key", "https://gw.example.com", "", ""},
		{"plain http", "http://gw.example.com", "k", ""},
		{"path prefix", "https://gw.example.com/fleet", "k", ""},
		{"query", "https://gw.example.com?x=1", "k", ""},
		{"no host", "https://", "k", ""},
	}
	for _, c := range cases {
		host, whyNot := registryHostFor(Config{PublicURL: c.publicURL, SessionKey: c.key})
		if host != c.want {
			t.Errorf("%s: host %q, want %q", c.name, host, c.want)
		}
		if (host == "") == (whyNot == "") {
			t.Errorf("%s: exactly one of host (%q) and reason (%q) must be set", c.name, host, whyNot)
		}
	}
}

// TestNewOffersRegistry: New turns the registry on only for a qualifying
// config, and then leaves Register handlers room for one registry connection
// per session.
func TestNewOffersRegistry(t *testing.T) {
	on, err := New(Config{PublicURL: "https://gw.example.com/", SessionKey: "k", MaxSessions: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !tunnel.HasFeature(on.supportedFeatures(), tunnel.FeatureRegistry) || on.reg.registryHost != "gw.example.com" {
		t.Fatalf("a qualifying gateway must offer the registry: %v %q", on.supportedFeatures(), on.reg.registryHost)
	}
	if cap(on.pendingSem) != 2*10+maxPendingHandshakes {
		t.Fatalf("pending handshakes cap %d, want %d", cap(on.pendingSem), 2*10+maxPendingHandshakes)
	}

	off, err := New(Config{PublicURL: "https://gw.example.com", MaxSessions: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tunnel.HasFeature(off.supportedFeatures(), tunnel.FeatureRegistry) || off.registryOff == "" {
		t.Fatalf("a gateway without a session key must not offer the registry: %v", off.supportedFeatures())
	}
	if cap(off.pendingSem) != 10+maxPendingHandshakes {
		t.Fatalf("pending handshakes cap %d, want %d (unchanged)", cap(off.pendingSem), 10+maxPendingHandshakes)
	}
	if got := off.supportedFeatures(); len(got) != len(gatewayFeatures) {
		t.Fatalf("supported features %v, want %v", got, gatewayFeatures)
	}
}

// A gateway that knows the registry feature but cannot offer it says why in
// the reply of a registration that asked for it (and only then), so the
// daemon's settings can name the cause instead of guessing.
func TestGatewayRegistryOffReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no session key", Config{PublicURL: "https://gw.example.com"}, "no --session-key"},
		{"plain http", Config{PublicURL: "http://gw.example.com", SessionKey: "k"}, "--public-url is not https"},
		{"path prefix", Config{PublicURL: "https://gw.example.com/fleet", SessionKey: "k"}, "--public-url has a path prefix"},
		{"offered", Config{PublicURL: "https://gw.example.com", SessionKey: "k"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.MaxSessions = 8
			gw, err := New(tc.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw.log = slog.New(slog.NewTextHandler(io.Discard, nil))
			publicLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()

			asked := connectFake(t, grpcLn.Addr().String(), insecure.NewCredentials(),
				tunnel.RegisterRequest{Features: []string{tunnel.FeatureRegistry, tunnel.FeatureWebhook}}, nil, mcpRoot)
			if asked.reply.Error != "" {
				t.Fatalf("register: %s", asked.reply.Error)
			}
			if asked.reply.RegistryOff != tc.want {
				t.Fatalf("RegistryOff = %q, want %q", asked.reply.RegistryOff, tc.want)
			}
			if negotiated := tunnel.HasFeature(asked.reply.Features, tunnel.FeatureRegistry); negotiated != (tc.want == "") || negotiated != (asked.reply.PublicRegistry != "") {
				t.Fatalf("features %v, public registry %q", asked.reply.Features, asked.reply.PublicRegistry)
			}
			notAsked := connectFake(t, grpcLn.Addr().String(), insecure.NewCredentials(),
				tunnel.RegisterRequest{Features: []string{tunnel.FeatureWebhook}}, nil, mcpRoot)
			if notAsked.reply.RegistryOff != "" {
				t.Fatalf("a registration that did not ask for the registry got RegistryOff %q", notAsked.reply.RegistryOff)
			}
		})
	}
}
