package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc/credentials"
)

// registry_minikube_test.go is the minikube test-CA trust check for gateway
// image delivery: a real `fleet gateway` (gateway.New + ServeListeners) serves
// TLS from a throwaway CA on the daemon host, a fleetd stand-in registers with
// FeatureRegistry and attaches a registry connection, and the minikube node's
// containerd pulls a tiny runnable image through <host>/v2/<rid>/... into a Pod.
//
// It needs a running minikube with the containerd runtime, kubectl, and Go, so
// it runs only when FLEET_GATEWAY_MINIKUBE names the minikube profile (its kube
// context has the same name):
//
//	FLEET_GATEWAY_MINIKUBE=fleet-k8s go test -run Minikube -v ./internal/gateway/
//
// The node trusts the CA through containerd's per-registry trust directory
// (/etc/containerd/certs.d/<host:port>/ca.crt, read on every pull), so nothing
// restarts and nothing outside this test's own registry host:port changes. The
// test also checks the pull fails before the CA is trusted, and that with the
// daemon gone a pull gets the gateway's 502 (reconnecting) and an unknown
// registry id 404. It cleans up its namespace, the trust directory and the
// pulled image.

// minikubeRun runs a command, failing the test on error, and returns its
// combined output.
func minikubeRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// nodeShell runs a shell command on the minikube node.
func nodeShell(profile, script string) (string, error) {
	out, err := exec.Command("minikube", "-p", profile, "ssh", "--", script).CombinedOutput()
	return string(out), err
}

// testCA is a throwaway CA and a server certificate it signed.
type testCA struct {
	caPEM             []byte
	certFile, keyFile string
	pool              *x509.CertPool
}

// newTestCA issues a server certificate for host and ip from a fresh CA.
func newTestCA(t *testing.T, host string, ip net.IP) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fleet gateway test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{ip},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	dir := t.TempDir()
	ca := testCA{
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certFile: filepath.Join(dir, "gateway.pem"),
		keyFile:  filepath.Join(dir, "gateway-key.pem"),
		pool:     x509.NewCertPool(),
	}
	ca.pool.AddCert(caCert)
	if err := os.WriteFile(ca.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(ca.keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return ca
}

// buildHello builds a static linux binary for arch that prints marker.
func buildHello(t *testing.T, arch, marker string) []byte {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH; it builds the test image's binary")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\""+marker+"\") }\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	out := filepath.Join(dir, "hello")
	cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", out, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	return bin
}

func TestGatewayRegistryMinikubePull(t *testing.T) {
	profile := os.Getenv("FLEET_GATEWAY_MINIKUBE")
	if profile == "" {
		t.Skip("set FLEET_GATEWAY_MINIKUBE=<minikube profile> to run the minikube pull check")
	}
	for _, tool := range []string{"minikube", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not on PATH", tool)
		}
	}
	kubectl := func(args ...string) string {
		t.Helper()
		return minikubeRun(t, "kubectl", append([]string{"--context", profile}, args...)...)
	}
	if rt := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.containerRuntimeVersion}"); !strings.HasPrefix(rt, "containerd://") {
		t.Skipf("node runtime is %q; this check drives containerd's trust directory", rt)
	}
	arch := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")

	// The daemon host as the node sees it: minikube maps host.minikube.internal
	// to it in the node's /etc/hosts, which containerd's resolver reads.
	const host = "host.minikube.internal"
	hostsLine, err := nodeShell(profile, "getent hosts "+host)
	if err != nil {
		t.Fatalf("resolve %s on the node: %v\n%s", host, err, hostsLine)
	}
	hostIP := net.ParseIP(strings.Fields(hostsLine)[0])
	if hostIP == nil {
		t.Fatalf("unparseable %s address: %q", host, hostsLine)
	}

	// The gateway: TLS from the test CA on the host-facing address, a session
	// key and an https public URL at the host root, so it offers the registry.
	ca := newTestCA(t, host, hostIP)
	rawPublic, err := net.Listen("tcp", net.JoinHostPort(hostIP.String(), "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", hostIP, err)
	}
	port := rawPublic.Addr().(*net.TCPAddr).Port
	registryHost := fmt.Sprintf("%s:%d", host, port)
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	sessionKey := make([]byte, 16)
	_, _ = rand.Read(sessionKey)
	gw, err := New(Config{
		PublicURL:  "https://" + registryHost,
		SessionKey: hex.EncodeToString(sessionKey),
		TLSCert:    ca.certFile,
		TLSKey:     ca.keyFile,
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	leaf, err := tls.LoadX509KeyPair(ca.certFile, ca.keyFile)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	publicLn := tls.NewListener(rawPublic, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()

	// The fleetd stand-in: a registration with the registry feature and a
	// registry connection, serving a one-layer image whose binary prints marker.
	// A per-run file makes the digest unique, so no image an earlier run left on
	// the node can stand in for this pull.
	const marker = "fleet-gateway-pull-ok"
	pass := hex.EncodeToString(sessionKey[:8])
	reg := &testRegistry{
		img: newTestImage(t, arch, []string{"/hello"},
			testFile{name: "hello", mode: 0o755, body: buildHello(t, arch, marker)},
			testFile{name: "run-id", mode: 0o644, body: sessionKey}),
		user: testRegistryUser, pass: pass,
	}
	creds := credentials.NewTLS(&tls.Config{RootCAs: ca.pool, ServerName: host})
	primary := connectFake(t, grpcLn.Addr().String(), creds, tunnel.RegisterRequest{Features: []string{tunnel.FeatureRegistry}},
		map[byte]http.Handler{tunnel.TagMCP: mcpRoot, tunnel.TagRegistry: reg.handler("main")}, nil)
	if primary.reply.Error != "" || !strings.HasPrefix(primary.reply.PublicRegistry, registryHost+"/") {
		t.Fatalf("register: error %q, PublicRegistry %q", primary.reply.Error, primary.reply.PublicRegistry)
	}
	waitRegistered(t, gw, publicIDOf(t, primary.reply.PublicURL))
	attached := connectFake(t, grpcLn.Addr().String(), creds,
		tunnel.RegisterRequest{Attach: tunnel.AttachRegistry, SessionID: primary.reply.SessionID},
		map[byte]http.Handler{tunnel.TagRegistry: reg.handler("attach")}, nil)
	if attached.reply.Error != "" {
		t.Fatalf("attach: %s", attached.reply.Error)
	}
	ref := primary.reply.PublicRegistry + "/devcontainer@" + reg.img.manifestDigest
	t.Logf("image reference %s", ref)

	crictlPull := func(image string) (string, error) {
		return nodeShell(profile, "sudo crictl pull --creds "+testRegistryUser+":"+pass+" "+image)
	}
	t.Cleanup(func() { _, _ = nodeShell(profile, "sudo crictl rmi "+ref) })

	// Before the node trusts the CA, the pull fails on the certificate.
	if out, err := crictlPull(ref); err == nil || !strings.Contains(out, "certificate") {
		t.Fatalf("a pull before the CA is trusted must fail on the certificate, got err=%v:\n%s", err, out)
	}
	if hits := reg.seen(); len(hits) != 0 {
		t.Fatalf("an untrusted pull reached the daemon: %+v", hits)
	}

	// Trust the test CA for this registry host:port only.
	trustDir := "/etc/containerd/certs.d/" + registryHost
	caB64 := base64.StdEncoding.EncodeToString(ca.caPEM)
	if out, err := nodeShell(profile, "sudo mkdir -p "+trustDir+" && echo "+caB64+" | base64 -d | sudo tee "+trustDir+"/ca.crt >/dev/null"); err != nil {
		t.Fatalf("install the CA on the node: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = nodeShell(profile, "sudo rm -rf "+trustDir) })

	// A Pod pulls through the gateway with a pull secret keyed by host:port,
	// and its image runs.
	ns := "fleet-it-p2-07-gateway-" + hex.EncodeToString(sessionKey[8:11])
	kubectl("create", "namespace", ns)
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", profile, "delete", "namespace", ns, "--wait=false").Run()
	})
	kubectl("-n", ns, "create", "secret", "docker-registry", "gateway-pull",
		"--docker-server="+registryHost, "--docker-username="+testRegistryUser, "--docker-password="+pass)
	pod := `apiVersion: v1
kind: Pod
metadata:
  name: gateway-pull
spec:
  restartPolicy: Never
  imagePullSecrets: [{name: gateway-pull}]
  containers:
  - name: hello
    image: ` + ref + `
    imagePullPolicy: IfNotPresent
`
	apply := exec.Command("kubectl", "--context", profile, "-n", ns, "apply", "-f", "-")
	apply.Stdin = strings.NewReader(pod)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply pod: %v\n%s", err, out)
	}
	if out, err := exec.Command("kubectl", "--context", profile, "-n", ns, "wait", "pod/gateway-pull",
		"--for=jsonpath={.status.phase}=Succeeded", "--timeout=180s").CombinedOutput(); err != nil {
		events, _ := exec.Command("kubectl", "--context", profile, "-n", ns, "get", "events").CombinedOutput()
		t.Fatalf("pod never succeeded: %v\n%s\n%s", err, out, events)
	}
	if logs := kubectl("-n", ns, "logs", "gateway-pull"); !strings.Contains(logs, marker) {
		t.Fatalf("pod logs %q, want %q", logs, marker)
	}

	// Every request reached the daemon over the registry connection with the
	// pull secret's credential, rid stripped, and the image was fetched whole.
	auth := basicAuth(testRegistryUser, pass)
	paths := map[string]bool{}
	for _, h := range reg.seen() {
		if h.via != "attach" {
			t.Errorf("request %s %s went via %s, want the registry connection", h.method, h.path, h.via)
		}
		if h.auth == auth {
			paths[h.path] = true
		}
	}
	want := []string{"/v2/devcontainer/manifests/" + reg.img.manifestDigest}
	for d := range reg.img.blobs {
		want = append(want, "/v2/devcontainer/blobs/"+d)
	}
	for _, p := range want {
		if !paths[p] {
			t.Errorf("the node never fetched %s with the pull credential; saw %+v", p, reg.seen())
		}
	}
	t.Logf("the node pulled %d objects through the gateway over the registry connection (%d requests)", len(want), len(reg.seen()))

	// An unknown registry id: the gateway's 404.
	if out, err := crictlPull(registryHost + "/" + hexID(4) + "/devcontainer@" + reg.img.manifestDigest); err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("a pull of an unknown registry id must fail as not found, got err=%v:\n%s", err, out)
	}

	// The daemon drops off (both connections): within the session TTL the
	// gateway answers 502, the signal fleetd later classifies as "daemon was
	// offline".
	attached.close()
	primary.close()
	sess := gw.reg.lookupRegistry(registryIDOf(t, primary.reply.PublicRegistry, registryHost))
	waitFor(t, "the tunnel to close", func() bool { return sess.currentYamux().IsClosed() })
	if out, err := crictlPull(ref); err == nil || !strings.Contains(out, "502") {
		t.Fatalf("a pull while the daemon is away must get the gateway's 502, got err=%v:\n%s", err, out)
	}
}
