//go:build !nok8s

package server

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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// kube_image_minikube_test.go is `fleet cluster test --deep` for Gateway
// delivery on a real cluster, through fleetd's own wiring (startImages, the
// tunnel's registry listener, the connection's preflight): a local gateway
// serves TLS from a throwaway CA that the minikube node's containerd trusts
// (/etc/containerd/certs.d/<host:port>/ca.crt), fleetd's tunnel client
// registers with Enable Image Registry alone, and the connection, set to
// Gateway delivery, is tested deep:
//
//  1. the daemon-side manifest check passes and a pod pulls the probe image
//     through the gateway;
//  2. with the CA gone from the node, the pod's pull fails at the TLS step;
//  3. with the gateway stopped (fleetd running), the daemon-side check and
//     the pod's pull both fail at the route step.
//
//	FLEET_KUBE_GATEWAY_MINIKUBE=fleet-k8s go test -count=1 -timeout 20m -run Minikube -v ./internal/server/
//
// It needs minikube (docker driver, containerd), kubectl, and the context of
// the profile's name in ~/.kube/config; it creates and deletes a namespace
// named fleet-it-p2-09-gateway-delivery-* and the trust directory.

func TestKubeGatewayDeepTestMinikube(t *testing.T) {
	profile := os.Getenv("FLEET_KUBE_GATEWAY_MINIKUBE")
	if profile == "" {
		t.Skip("set FLEET_KUBE_GATEWAY_MINIKUBE=<minikube profile> to test Gateway delivery on minikube")
	}
	for _, tool := range []string{"minikube", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not on PATH", tool)
		}
	}
	home, _ := os.UserHomeDir()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
		t.Setenv("KUBECONFIG", kubeconfig)
	}
	if os.Getenv("MINIKUBE_HOME") == "" {
		t.Setenv("MINIKUBE_HOME", filepath.Join(home, ".minikube"))
	}
	isolateFleetDir(t)
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("kubectl", append([]string{"--context", profile}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	node := func(script string) (string, error) {
		out, err := exec.Command("minikube", "-p", profile, "ssh", "--", script).CombinedOutput()
		return string(out), err
	}
	if rt := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.containerRuntimeVersion}"); !strings.HasPrefix(rt, "containerd://") {
		t.Skipf("node runtime is %q; this test drives containerd's trust directory", rt)
	}

	// The gateway, on the daemon host as the node sees it.
	const host = "host.minikube.internal"
	line, err := node("getent hosts " + host)
	if err != nil {
		t.Fatalf("resolve %s on the node: %v\n%s", host, err, line)
	}
	hostIP := net.ParseIP(strings.Fields(line)[0])
	caPEM, leaf, pool := minikubeTestCA(t, host, hostIP)
	rawPublic, err := net.Listen("tcp", net.JoinHostPort(hostIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	publicAddr := rawPublic.Addr().String()
	registryHost := fmt.Sprintf("%s:%d", host, rawPublic.Addr().(*net.TCPAddr).Port)
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{PublicURL: "https://" + registryHost, SessionKey: "p2-09-minikube", MaxSessions: 8})
	if err != nil {
		t.Fatal(err)
	}
	gwCtx, stopGateway := context.WithCancel(context.Background())
	gwDone := make(chan struct{})
	go func() {
		defer close(gwDone)
		_ = gw.ServeListeners(gwCtx, tls.NewListener(rawPublic, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}), grpcLn)
	}()
	t.Cleanup(func() { stopGateway(); <-gwDone })

	// fleetd: the kube manager with image delivery, and the tunnel client
	// with Enable Image Registry alone.
	svc := newService()
	m := svc.kube
	m.environ = func() []string { return append(os.Environ(), "KUBECONFIG="+kubeconfig) }
	m.loadIdentity = liveIdentity
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go svc.hub.run(ctx)
	opts := m.startImages(ctx, true)
	// The daemon host resolves the gateway's name to the address it serves
	// on and trusts the test CA, as a real daemon host would trust a public
	// certificate.
	m.images.gateway.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, publicAddr)
		},
	}
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "mcp") }))
	t.Cleanup(mcp.Close)
	mcpPort, _ := strconv.Atoi(mcp.URL[strings.LastIndex(mcp.URL, ":")+1:])
	statuses := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	mgr := remote.NewManager(mcpPort, "vtest", func(st *fleetgrpc.RemoteMcpStatus) {
		select {
		case statuses <- st:
		default:
		}
	}, append(opts, remote.WithDialFunc(h2cRegisterDial(grpcLn.Addr().String())))...)
	m.attachTunnel(mgr)
	go mgr.Run(ctx)
	mgr.Reconcile(false, false, false, true, "https://"+registryHost)
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the registry prefix", func() bool { p, _ := mgr.Registry(); return p != "" })
	prefix, _ := mgr.Registry()
	if !strings.HasPrefix(prefix, registryHost+"/") {
		t.Fatalf("registry prefix %q, want %s/<rid>", prefix, registryHost)
	}

	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	ns := "fleet-it-p2-09-gateway-delivery-" + hex.EncodeToString(nonce)
	kubectl("create", "namespace", ns)
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", profile, "delete", "namespace", ns, "--wait=false").Run()
	})
	trustDir := "/etc/containerd/certs.d/" + registryHost
	trust := func() {
		t.Helper()
		if out, err := node("sudo mkdir -p " + trustDir + " && echo " + base64.StdEncoding.EncodeToString(caPEM) + " | base64 -d | sudo tee " + trustDir + "/ca.crt >/dev/null"); err != nil {
			t.Fatalf("install the CA on the node: %v\n%s", err, out)
		}
	}
	untrust := func() { _, _ = node("sudo rm -rf " + trustDir) }
	t.Cleanup(untrust)
	trust()

	added, err := svc.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{
		Context: profile, Namespace: ns, DisplayName: "p2-09-gateway", Image: &fleetgrpc.KubeImageSettings{Mode: "gateway"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	id := added.GetCluster().GetCluster().GetId()
	deep := func() *fleetgrpc.KubeClusterStatus {
		t.Helper()
		st, err := m.test(ctx, id, fullRun{deep: true}, func(c *fleetgrpc.KubeCheck) {
			if c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING && strings.HasPrefix(c.GetName(), "image_") {
				t.Logf("  %s %s: %s %s", c.GetName(), strings.TrimPrefix(c.GetResult().String(), "KUBE_CHECK_RESULT_"), c.GetDetail(), c.GetFix())
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	result := func(st *fleetgrpc.KubeClusterStatus, name string) *fleetgrpc.KubeCheck {
		t.Helper()
		for _, c := range st.GetChecks() {
			if c.GetName() == name {
				return c
			}
		}
		t.Fatalf("no %s check: %v", name, st.GetChecks())
		return nil
	}

	// 1. Trusted and running: the route and a real pull through it.
	st := deep()
	if c := result(st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS || !strings.Contains(c.GetDetail(), "route + credential ok ("+registryHost+")") {
		t.Fatalf("daemon-side check %v", c)
	}
	if c := result(st, preflight.CheckImagePull); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
		t.Fatalf("deep pull %v", c)
	}
	if st.GetDeepCheckedAt() == nil || st.GetCapabilities().GetImageDelivery().GetMode() != "gateway" {
		t.Fatalf("status %v", st)
	}
	kubectl("-n", ns, "get", "secret", "fleet-gwpull-"+liveOwner()+"-"+id)
	waitFor("the pull pod's deletion", func() bool {
		return kubectl("-n", ns, "get", "pods", "-l", "fleet.dev/role=probe", "-o", "name") == ""
	})

	// 2. The node no longer trusts the gateway's certificate (the daemon
	// host still does): the pod's pull names TLS.
	untrust()
	st = deep()
	if c := result(st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS {
		t.Fatalf("daemon-side check without node trust %v", c)
	}
	if c := result(st, preflight.CheckImagePull); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(c.GetDetail(), "tls: ") {
		t.Fatalf("a pull the node cannot trust %v", c)
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED || !strings.Contains(st.GetMessage(), "gateway pull failing (tls)") {
		t.Fatalf("state %v %q", st.GetState(), st.GetMessage())
	}

	// 3. fleetd running, the gateway stopped before a fresh pull: both
	// checks name the route, and the pull's error is the classified one.
	trust()
	stopGateway()
	<-gwDone
	st = deep()
	if c := result(st, preflight.CheckImageDelivery); c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(c.GetDetail(), "gateway route: ") {
		t.Fatalf("daemon-side check with the gateway stopped %v", c)
	}
	c := result(st, preflight.CheckImagePull)
	if c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL || !strings.HasPrefix(c.GetDetail(), "route: ") || !strings.Contains(c.GetDetail(), "could not reach the gateway") {
		t.Fatalf("a pull with the gateway stopped %v", c)
	}
	if st.GetCapabilities().GetImageDelivery().GetGatewayFailedStep() != "route" {
		t.Fatalf("failed step %q", st.GetCapabilities().GetImageDelivery().GetGatewayFailedStep())
	}
}

// liveIdentity is the test daemon's identity for the live test.
func liveIdentity() (owner.Identity, error) { return testIdentity, nil }

func liveOwner() string { return testIdentity.Owner() }

// h2cRegisterDial opens the gateway's Register stream on its plaintext
// gRPC listener (the public listener is the one nodes reach over TLS).
func h2cRegisterDial(addr string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		stream, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, tunnel.RegisterMethod, grpc.ForceCodec(tunnel.RawCodec{}))
		if err != nil {
			_ = cc.Close()
			return nil, err
		}
		return tunnel.NewStreamConn(stream, func() { _ = cc.Close() }), nil
	}
}

// minikubeTestCA is a throwaway CA and a server certificate for host and ip
// it signed: the CA's PEM, the certificate and a pool holding the CA.
func minikubeTestCA(t *testing.T, host string, ip net.IP) ([]byte, tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet p2-09 test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(3 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(3 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{host}, IPAddresses: []net.IP{ip}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leaf, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), leaf, pool
}
