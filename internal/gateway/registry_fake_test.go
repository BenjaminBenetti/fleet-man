package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc/credentials"
)

// registry_fake_test.go holds the test doubles for the registry route: a
// fleetd stand-in that registers (or attaches) with chosen features and serves
// tagged streams, and a minimal pull-only OCI registry serving one image. The
// registry is a TEST stand-in for fleetd's image server, enough for a real
// container runtime to pull from (registry_minikube_test.go).

// startRegistryGateway starts a TLS gateway on ephemeral listeners that offers
// the registry feature, as New does for a gateway with --session-key and an
// https, path-free --public-url (here "https://gw.example.com").
func startRegistryGateway(t *testing.T, cert tls.Certificate, sessionKey string, maxSessions int) (*Server, string, string) {
	t.Helper()
	const publicBase = "https://gw.example.com"
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	reg := newRegistry(publicBase, "", maxSessions, testSigner(t, sessionKey))
	reg.registryHost = "gw.example.com"
	s := &Server{
		cfg:       Config{PublicURL: publicBase, MaxSessions: maxSessions, SessionKey: sessionKey},
		reg:       reg,
		tlsConfig: tlsCfg,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.ServeListeners(ctx, publicLn, grpcLn) }()
	return s, publicLn.Addr().String(), grpcLn.Addr().String()
}

func tlsCredsFor(pool *x509.CertPool) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
}

// fakeDaemon is one fleetd-side connection to the gateway: a registration or
// a registry attach, with the yamux session it serves.
type fakeDaemon struct {
	reply tunnel.RegisterReply
	ym    *yamux.Session
	conn  net.Conn
}

// close drops the connection, as a daemon that went away would.
func (d *fakeDaemon) close() {
	_ = d.ym.Close()
	_ = d.conn.Close()
}

// connectFake opens a Register stream, sends req, and on success serves the
// returned routes (tag → handler) over the connection's yamux session: the
// registry yamux config for an attach, the main one otherwise. tagged selects
// tag demuxing; an untagged connection serves every stream with untagged.
// A refused handshake returns the reply with a nil session.
func connectFake(t *testing.T, grpcAddr string, creds credentials.TransportCredentials, req tunnel.RegisterRequest, routes map[byte]http.Handler, untagged http.Handler) *fakeDaemon {
	t.Helper()
	conn := openRegisterStream(t, grpcAddr, creds)
	reply, err := tunnel.Handshake(conn, req, 5*time.Second)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if reply.Error != "" {
		_ = conn.Close()
		return &fakeDaemon{reply: reply}
	}
	session := tunnel.ClientSession
	if req.Attach != "" {
		session = tunnel.RegistryClientSession
	}
	ym, err := session(conn, io.Discard)
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	d := &fakeDaemon{reply: reply, ym: ym, conn: conn}
	t.Cleanup(d.close)
	if untagged != nil && !tunnel.Tagged(reply.Features) && req.Attach == "" {
		srv := &http.Server{Handler: untagged}
		go func() { _ = srv.Serve(ym) }()
		t.Cleanup(func() { _ = srv.Close() })
		return d
	}
	listeners := map[byte]*pushListener{}
	for tag, h := range routes {
		l := newPushListener()
		listeners[tag] = l
		srv := &http.Server{Handler: h}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	go func() {
		for {
			stream, err := ym.Accept()
			if err != nil {
				return
			}
			go func() {
				tag, err := tunnel.ReadTag(stream)
				if l := listeners[tag]; err == nil && l != nil {
					l.push(stream)
					return
				}
				_ = stream.Close()
			}()
		}
	}()
	return d
}

// pushListener is a net.Listener fed by push (the test's copy of fleetd's
// ChanListener, which lives in a server-only package).
type pushListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPushListener() *pushListener {
	return &pushListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pushListener) push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *pushListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pushListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pushListener) Addr() net.Addr { return &net.TCPAddr{} }

// testImage is a single-layer OCI image: its manifest (served by digest) and
// every blob it references.
type testImage struct {
	manifest       []byte
	manifestDigest string
	blobs          map[string][]byte
}

// testFile is one regular file in a test image layer.
type testFile struct {
	name string
	mode int64
	body []byte
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newTestImage builds an OCI image whose one gzip layer holds files, for arch
// (a GOARCH name) with entrypoint.
func newTestImage(t *testing.T, arch string, entrypoint []string, files ...testFile) testImage {
	t.Helper()
	var layerTar bytes.Buffer
	tw := tar.NewWriter(&layerTar)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	var layer bytes.Buffer
	gz := gzip.NewWriter(&layer)
	if _, err := gz.Write(layerTar.Bytes()); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	config, err := json.Marshal(map[string]any{
		"architecture": arch,
		"os":           "linux",
		"config":       map[string]any{"Entrypoint": entrypoint},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{digestOf(layerTar.Bytes())}},
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    digestOf(config), "size": len(config),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    digestOf(layer.Bytes()), "size": layer.Len(),
		}},
	})
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return testImage{
		manifest:       manifest,
		manifestDigest: digestOf(manifest),
		blobs: map[string][]byte{
			digestOf(config):        config,
			digestOf(layer.Bytes()): layer.Bytes(),
		},
	}
}

// registryHit is one request the test registry saw.
type registryHit struct {
	via, method, path, auth, rangeHdr string
}

// testRegistry is a pull-only OCI registry for one repository ("devcontainer")
// holding one image, behind a Basic credential, as fleetd's image server is.
// Every request is recorded with the connection it arrived on (via).
type testRegistry struct {
	img        testImage
	user, pass string

	mu   sync.Mutex
	hits []registryHit
	// hold, when set, makes blob requests wait until it is closed (and counts
	// how many wait at once), for the stream-cap test.
	hold     chan struct{}
	inFlight int
	peak     int
	// singleWrite serves a whole blob (no Range) in one Write, so the tunnel
	// sees writes as large as the stream window allows.
	singleWrite bool
}

func (r *testRegistry) seen() []registryHit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registryHit(nil), r.hits...)
}

func (r *testRegistry) handler(via string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, registryHit{via: via, method: req.Method, path: req.URL.Path, auth: req.Header.Get("Authorization"), rangeHdr: req.Header.Get("Range")})
		r.mu.Unlock()

		if user, pass, ok := req.BasicAuth(); !ok || user != r.user || pass != r.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="fleet"`)
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		rest, ok := strings.CutPrefix(req.URL.Path, "/v2/devcontainer/")
		if !ok {
			writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown repository")
			return
		}
		switch kind, ref, _ := strings.Cut(rest, "/"); kind {
		case "manifests":
			if ref != r.img.manifestDigest {
				writeRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "unknown manifest")
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", r.img.manifestDigest)
			http.ServeContent(w, req, "", time.Time{}, bytes.NewReader(r.img.manifest))
		case "blobs":
			blob, ok := r.img.blobs[ref]
			if !ok {
				writeRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "unknown blob")
				return
			}
			if r.hold != nil {
				r.mu.Lock()
				r.inFlight++
				r.peak = max(r.peak, r.inFlight)
				r.mu.Unlock()
				<-r.hold
				r.mu.Lock()
				r.inFlight--
				r.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", ref)
			if r.singleWrite && req.Header.Get("Range") == "" {
				w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
				_, _ = w.Write(blob)
				return
			}
			http.ServeContent(w, req, "", time.Time{}, bytes.NewReader(blob))
		default:
			writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown path")
		}
	})
}

// mcpRoot is a minimal MCP stand-in: every request answers "root".
var mcpRoot = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "root") })

func hexOf(b []byte) string { return hex.EncodeToString(b) }
