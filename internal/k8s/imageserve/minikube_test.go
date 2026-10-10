package imageserve_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// minikube_test.go is gateway delivery on a real cluster: a local gateway
// serves TLS from a throwaway CA that the minikube node's containerd trusts
// (/etc/containerd/certs.d/<host:port>/ca.crt, read on every pull), fleetd's
// real tunnel client attaches its registry connection, the image server
// serves an image the host's docker built, the managed gateway pull Secret
// is applied server-side, and a Pod pulls it and runs. Then, with fleetd
// running, the gateway stops before a fresh pull and the pod's failure is
// classified as the gateway being unreachable. With
// FLEET_IMAGESERVE_MINIKUBE_REAP=1 it also takes fleetd offline for more
// than the gateway's 10-minute session TTL (about 11 minutes) and checks
// that the resulting "not found" is classified as the daemon having been
// offline, and that the pull works once fleetd is back.
//
//	FLEET_IMAGESERVE_MINIKUBE=fleet-k8s go test -count=1 -timeout 30m -run Minikube -v ./internal/k8s/imageserve/
//
// It needs minikube (docker driver, containerd runtime), kubectl and the
// host's docker, and cleans up its namespace, the trust directory and the
// images it pulled on the node.

// kubectlSecrets applies pull Secrets with kubectl, server-side, as the
// cluster backend does with client-go.
type kubectlSecrets struct{ context, namespace string }

func (k kubectlSecrets) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", k.context, "-n", k.namespace}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

func (k kubectlSecrets) ApplyPullSecret(ctx context.Context, s kubeimage.PullSecret) error {
	b, err := s.Manifest()
	if err != nil {
		return err
	}
	_, err = k.run(ctx, b, "apply", "--server-side", "--field-manager=fleet", "-f", "-")
	return err
}

func (k kubectlSecrets) DeletePullSecret(ctx context.Context, name string) error {
	_, err := k.run(ctx, nil, "delete", "secret", name, "--ignore-not-found")
	return err
}

func nodeShell(profile, script string) (string, error) {
	out, err := exec.Command("minikube", "-p", profile, "ssh", "--", script).CombinedOutput()
	return string(out), err
}

// testCA is a throwaway CA and a server certificate it signed.
type testCA struct {
	caPEM             []byte
	certFile, keyFile string
	pool              *x509.CertPool
	leaf              tls.Certificate
}

func newTestCA(t *testing.T, host string, ip net.IP) testCA {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet imageserve test CA"},
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
	dir := t.TempDir()
	ca := testCA{caPEM: pemBlock("CERTIFICATE", caDER), certFile: filepath.Join(dir, "gw.pem"), keyFile: filepath.Join(dir, "gw-key.pem"), pool: x509.NewCertPool()}
	ca.pool.AddCert(caCert)
	_ = os.WriteFile(ca.certFile, pemBlock("CERTIFICATE", leafDER), 0o600)
	_ = os.WriteFile(ca.keyFile, pemBlock("EC PRIVATE KEY", keyDER), 0o600)
	if ca.leaf, err = tls.LoadX509KeyPair(ca.certFile, ca.keyFile); err != nil {
		t.Fatal(err)
	}
	return ca
}

func pemBlock(typ string, der []byte) []byte {
	return []byte("-----BEGIN " + typ + "-----\n" + wrap64(base64.StdEncoding.EncodeToString(der)) + "-----END " + typ + "-----\n")
}

func wrap64(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	b.WriteString(s + "\n")
	return b.String()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// podWaiting is a container's waiting state.
type podWaiting struct{ Reason, Message string }

func TestGatewayDeliveryMinikube(t *testing.T) {
	profile := os.Getenv("FLEET_IMAGESERVE_MINIKUBE")
	if profile == "" {
		t.Skip("set FLEET_IMAGESERVE_MINIKUBE=<minikube profile> to run gateway delivery on minikube")
	}
	for _, tool := range []string{"minikube", "kubectl", "docker"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not on PATH", tool)
		}
	}
	// fleetd's files go to a temp HOME; kubectl and minikube keep the real
	// one's configuration.
	home, _ := os.UserHomeDir()
	if os.Getenv("KUBECONFIG") == "" {
		t.Setenv("KUBECONFIG", filepath.Join(home, ".kube", "config"))
	}
	if os.Getenv("MINIKUBE_HOME") == "" {
		t.Setenv("MINIKUBE_HOME", filepath.Join(home, ".minikube"))
	}
	if os.Getenv("DOCKER_CONFIG") == "" {
		t.Setenv("DOCKER_CONFIG", filepath.Join(home, ".docker"))
	}
	t.Setenv("HOME", t.TempDir())
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("kubectl", append([]string{"--context", profile}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if rt := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.containerRuntimeVersion}"); !strings.HasPrefix(rt, "containerd://") {
		t.Skipf("node runtime is %q; this test drives containerd's trust directory", rt)
	}
	arch := kubectl("get", "nodes", "-o", "jsonpath={.items[0].status.nodeInfo.architecture}")

	// The daemon host as the node sees it.
	const host = "host.minikube.internal"
	line, err := nodeShell(profile, "getent hosts "+host)
	if err != nil {
		t.Fatalf("resolve %s on the node: %v\n%s", host, err, line)
	}
	hostIP := net.ParseIP(strings.Fields(line)[0])
	ca := newTestCA(t, host, hostIP)
	rawPublic, err := net.Listen("tcp", net.JoinHostPort(hostIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := rawPublic.Addr().(*net.TCPAddr).Port
	registryHost := fmt.Sprintf("%s:%d", host, port)
	gwCfg := gateway.Config{PublicURL: "https://" + registryHost, SessionKey: "imageserve-minikube", TLSCert: ca.certFile, TLSKey: ca.keyFile}

	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(logs, os.Stderr), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcAddr := grpcLn.Addr().String()
	_, _, stopGateway := startGateway(t, gwCfg, tls.NewListener(rawPublic, &tls.Config{Certificates: []tls.Certificate{ca.leaf}, MinVersion: tls.VersionTLS12}), grpcLn)

	storeDir := t.TempDir()
	dial := registerDial(grpcAddr, tlsCreds(ca.pool, host))
	f := startFleetd(t, fleetdOptions{dial: dial, gatewayURL: gwCfg.PublicURL, storeDir: storeDir, realDocker: true})
	prefix := f.waitPrefix(t)
	if !strings.HasPrefix(prefix, registryHost+"/") {
		t.Fatalf("registry prefix %q, want %s/<rid>", prefix, registryHost)
	}

	// Trust the CA for this host:port only.
	trustDir := "/etc/containerd/certs.d/" + registryHost
	if out, err := nodeShell(profile, "sudo mkdir -p "+trustDir+" && echo "+base64.StdEncoding.EncodeToString(ca.caPEM)+" | base64 -d | sudo tee "+trustDir+"/ca.crt >/dev/null"); err != nil {
		t.Fatalf("install the CA on the node: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = nodeShell(profile, "sudo rm -rf "+trustDir) })

	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	ns := "fleet-it-p2-08-" + hex.EncodeToString(nonce)
	kubectl("create", "namespace", ns)
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context", profile, "delete", "namespace", ns, "--wait=false").Run()
	})
	target := f.target("kc-1")
	target.Platform = "linux/" + arch
	target.Secrets = kubectlSecrets{context: profile, namespace: ns}

	// buildAndDeliver builds an image printing marker (busybox base, so the
	// node also pulls the base layers through the gateway) and delivers it.
	buildAndDeliver := func(marker string) kubeimage.GatewayDelivery {
		t.Helper()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "marker"), []byte(marker+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM busybox:musl\nCOPY marker /marker\nCMD [\"cat\", \"/marker\"]\n"), 0o644)
		tag := "fleet-imageserve-minikube:" + marker
		if out, err := exec.Command("docker", "build", "-q", "--platform", "linux/"+arch, "-t", tag, dir).CombinedOutput(); err != nil {
			t.Fatalf("docker build: %v\n%s", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })
		id := dockerOut(t, "image", "inspect", "--format", "{{.Id}}", tag)
		d, err := f.images.Deliver(context.Background(), target, kubeimage.Image{Ref: tag, Config: kubeshim.ImageConfig{ID: id}, Built: true})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		t.Cleanup(func() { _, _ = nodeShell(profile, "sudo crictl rmi "+d.Ref) })
		return d
	}
	runPod := func(name string, d kubeimage.GatewayDelivery) {
		t.Helper()
		pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  imagePullSecrets: [{name: %s}]
  containers:
  - name: dev
    image: %s
    imagePullPolicy: IfNotPresent
`, name, d.PullSecret, d.Ref)
		apply := exec.Command("kubectl", "--context", profile, "-n", ns, "apply", "-f", "-")
		apply.Stdin = strings.NewReader(pod)
		if out, err := apply.CombinedOutput(); err != nil {
			t.Fatalf("apply pod: %v\n%s", err, out)
		}
	}
	waitSucceeded := func(name, marker string) {
		t.Helper()
		if out, err := exec.Command("kubectl", "--context", profile, "-n", ns, "wait", "pod/"+name, "--for=jsonpath={.status.phase}=Succeeded", "--timeout=240s").CombinedOutput(); err != nil {
			events, _ := exec.Command("kubectl", "--context", profile, "-n", ns, "get", "events").CombinedOutput()
			t.Fatalf("pod %s never succeeded: %v\n%s\n%s", name, err, out, events)
		}
		if got := kubectl("-n", ns, "logs", name); !strings.Contains(got, marker) {
			t.Fatalf("pod %s logs %q, want %q", name, got, marker)
		}
	}
	waitPullFailure := func(name string) podWaiting {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			out := kubectl("-n", ns, "get", "pod", name, "-o", "jsonpath={.status.containerStatuses[0].state.waiting}")
			var w podWaiting
			if out != "" && json.Unmarshal([]byte(out), &w) == nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") && w.Message != "" {
				return w
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("pod %s never failed its pull", name)
		return podWaiting{}
	}

	// 1. A fresh pull through the gateway, over the registry connection.
	m1 := "gw-ok-" + hex.EncodeToString(nonce)
	d1 := buildAndDeliver(m1)
	runPod("pull-1", d1)
	waitSucceeded("pull-1", m1)
	if p, ok := f.images.Handler().LastPull("kc-1"); !ok || p.Manifest != d1.Digest || p.Bytes == 0 {
		t.Fatalf("the image server saw no pull of %s: %+v", d1.Digest, p)
	}
	if !strings.Contains(logs.String(), "gateway: registry connection attached") || f.attachBytes.Load() == 0 {
		t.Fatalf("the pull did not use the registry connection (%d bytes)", f.attachBytes.Load())
	}
	t.Logf("pulled %s through the gateway: %d bytes over the registry connection", d1.Ref, f.attachBytes.Load())

	// 2. fleetd running, the gateway stopped before a fresh pull: the node
	// cannot reach the gateway, and the failure says so.
	m2 := "gw-down-" + hex.EncodeToString(nonce)
	d2 := buildAndDeliver(m2)
	stopGateway()
	runPod("pull-2", d2)
	w := waitPullFailure("pull-2")
	pf, ok := f.images.ClassifyPull("kc-1", d2.Ref, w.Reason, w.Message)
	t.Logf("gateway stopped: kubelet said %s: %s\n  -> %s: %s", w.Reason, w.Message, pf.Kind, pf.Message)
	if !ok || pf.Kind != imageserve.PullGatewayUnreachable {
		t.Fatalf("a pull with the gateway stopped classified as %+v", pf)
	}

	if os.Getenv("FLEET_IMAGESERVE_MINIKUBE_REAP") == "" {
		t.Log("set FLEET_IMAGESERVE_MINIKUBE_REAP=1 to check a pull more than 10 minutes after fleetd went offline")
		return
	}

	// 3. The gateway is back (same port, key and certificate): fleetd
	// reconnects with its token and keeps its registry id.
	rawPublic2, err := net.Listen("tcp", net.JoinHostPort(hostIP.String(), fmt.Sprint(port)))
	if err != nil {
		t.Fatal(err)
	}
	grpcLn2, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		t.Fatal(err)
	}
	f.drainStatuses()
	startGateway(t, gwCfg, tls.NewListener(rawPublic2, &tls.Config{Certificates: []tls.Certificate{ca.leaf}, MinVersion: tls.VersionTLS12}), grpcLn2)
	f.waitConnected(t, 3*time.Minute)
	if p, _ := f.mgr.Registry(); p != prefix {
		t.Fatalf("the registry prefix changed across a gateway restart: %q -> %q", prefix, p)
	}
	// A new pod pulls at once (the failed one waits out the kubelet's
	// back-off).
	runPod("pull-2b", d2)
	waitSucceeded("pull-2b", m2)

	// 4. fleetd goes offline for longer than the gateway keeps its session.
	m3 := "gw-reaped-" + hex.EncodeToString(nonce)
	d3 := buildAndDeliver(m3)
	f.stop()
	wait := 10*time.Minute + 45*time.Second
	t.Logf("fleetd offline; waiting %v for the gateway to forget its session", wait)
	time.Sleep(wait)
	runPod("pull-3", d3)
	w = waitPullFailure("pull-3")
	t.Logf("fleetd offline past the TTL: kubelet said %s: %s", w.Reason, w.Message)
	if !strings.Contains(strings.ToLower(w.Message), "not found") {
		t.Fatalf("expected the gateway's not found, got %q", w.Message)
	}

	// 5. fleetd is back: the same registry id, and the failure is the daemon
	// having been offline, not a digest that is gone.
	f2 := startFleetd(t, fleetdOptions{dial: dial, gatewayURL: gwCfg.PublicURL, storeDir: storeDir, realDocker: true, clusters: f.clusters})
	f2.waitConnected(t, time.Minute)
	if p, _ := f2.mgr.Registry(); p != prefix {
		t.Fatalf("fleetd came back with registry %q, want %q", p, prefix)
	}
	pf, ok = f2.images.ClassifyPull("kc-1", d3.Ref, w.Reason, w.Message)
	t.Logf("  -> %s: %s", pf.Kind, pf.Message)
	if !ok || pf.Kind != imageserve.PullDaemonOffline || !strings.Contains(pf.Message, "daemon was offline") || pf.Redeliver {
		t.Fatalf("a pull while fleetd was offline past the TTL classified as %+v", pf)
	}
	runPod("pull-3b", d3)
	waitSucceeded("pull-3b", m3)
}
