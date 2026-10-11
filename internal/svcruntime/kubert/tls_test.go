//go:build !nok8s

package kubert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// handshake runs a TLS handshake between a server presenting ca's server
// certificate, which requires a client certificate ca signed (as buildkitd
// does with --tlscacert), and a client presenting cred, which checks the
// server against cred's CA and server name, at the time at.
func handshake(t *testing.T, ca *buildkitCA, cred fleet.ServiceCredential, at time.Time) error {
	t.Helper()
	serverCert, err := tls.X509KeyPair([]byte(ca.ServerCert), []byte(ca.ServerKey))
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM([]byte(ca.CACert))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cred.CA)
	clientCert, err := tls.X509KeyPair(cred.Cert, cred.Key)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return at }
	sc, cc := net.Pipe()
	defer sc.Close()
	defer cc.Close()
	srv := tls.Server(sc, &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"h2"}, Time: clock})
	cli := tls.Client(cc, &tls.Config{Certificates: []tls.Certificate{clientCert}, RootCAs: roots, ServerName: cred.ServerName, NextProtos: []string{"h2"}, Time: clock})
	// Each side closes its end when it is done, so the other never waits
	// on a pipe no one reads (in TLS 1.3 the server judges the client's
	// certificate after the client's handshake returned: the client reads
	// on to take the server's alert).
	errs := make(chan error, 1)
	go func() {
		err := srv.Handshake()
		_ = sc.Close()
		errs <- err
	}()
	cerr := cli.Handshake()
	if cerr != nil {
		_ = cc.Close()
	} else {
		go func() { _, _ = io.Copy(io.Discard, cli) }()
	}
	serr := <-errs
	if serr != nil {
		return serr
	}
	return cerr
}

// TestBuildkitCertificatesTakeMutualTLS: an instance's certificate gets it
// past the server's mutual TLS for its whole week (a 6-day-old one too), and
// not once it expired; a certificate of another fleet's CA, or none of the
// fleet's, is refused, and so is a server posing with a client certificate.
// Its subject is the instance's key, and the server's certificate is for the
// names the instances reach it by.
func TestBuildkitCertificatesTakeMutualTLS(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := meta("alpha")
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ca, changed, err := ensureCA(testCluster, m, start)
	if err != nil || !changed {
		t.Fatalf("ensureCA = %v, changed %v", err, changed)
	}
	cred, err := ca.issueClient("key-alpha-i1", start)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := parseCert(cred.Cert)
	if cert.Subject.CommonName != "key-alpha-i1" || cred.NotAfter.Sub(cred.NotBefore) != 7*24*time.Hour || !cred.NotBefore.Equal(start.Add(-clientCertSkew)) ||
		cred.ServerName != m.name(names.SuffixBuildkit) || cred.Serial == "" || string(cred.CA) != ca.CACert {
		t.Fatalf("credential CN %q valid %v..%v server %q serial %q", cert.Subject.CommonName, cred.NotBefore, cred.NotAfter, cred.ServerName, cred.Serial)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("client certificate usages %v", cert.ExtKeyUsage)
	}
	server, _ := parseCert([]byte(ca.ServerCert))
	if strings.Join(server.DNSNames, " ") != strings.Join(m.serverNames(), " ") || m.serverNames()[2] != m.name(names.SuffixBuildkit)+".fleet.svc" {
		t.Fatalf("server certificate names %v", server.DNSNames)
	}

	for _, c := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"fresh", start, true},
		{"6 days old", start.Add(6 * 24 * time.Hour), true},
		{"expired", start.Add(7*24*time.Hour + time.Minute), false},
	} {
		if err := handshake(t, ca, cred, c.at); (err == nil) != c.ok {
			t.Errorf("%s: handshake %v, want ok=%v", c.name, err, c.ok)
		}
	}

	other, _, err := ensureCA(testCluster, meta("beta"), start)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.issueClient("key-beta-i1", start)
	if err != nil {
		t.Fatal(err)
	}
	foreign.CA = cred.CA // the instance trusts alpha's server, but holds beta's certificate
	if err := handshake(t, ca, foreign, start); err == nil {
		t.Error("a certificate of another fleet's CA got past the server")
	}
	// A server posing with a client certificate of the fleet's own.
	impostor := *ca
	impostor.ServerCert, impostor.ServerKey = string(cred.Cert), string(cred.Key)
	if err := handshake(t, &impostor, cred, start); err == nil {
		t.Error("a client certificate served as the server's")
	}
}

// TestEnsureCA: the CA is made once and kept (0600, in a 0700 folder, on the
// daemon host); the server's certificate is issued again near its end (the
// CA kept, the instances' records too), the CA near its own (the records
// go); a file that does not read is made again.
func TestEnsureCA(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := meta("alpha")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ca, changed, err := ensureCA(testCluster, m, at)
	if err != nil || !changed {
		t.Fatalf("first = %v %v", changed, err)
	}
	path := state.KubeBuildkitCAPath(testCluster, m.Hash)
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("CA file %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("CA folder mode %v", st.Mode().Perm())
	}
	again, changed, err := ensureCA(testCluster, m, at.Add(time.Hour))
	if err != nil || changed || again.CACert != ca.CACert || again.ServerCert != ca.ServerCert {
		t.Fatalf("again: changed %v %v", changed, err)
	}

	ca.Installed = map[string]installedCert{"key-alpha-i1": {Instance: "i1"}}
	if err := saveCA(testCluster, m.Hash, ca); err != nil {
		t.Fatal(err)
	}
	renewed, changed, err := ensureCA(testCluster, m, at.Add(serverCertTTL-serverCertRenew+time.Hour))
	if err != nil || !changed || renewed.CACert != ca.CACert || renewed.ServerCert == ca.ServerCert || len(renewed.Installed) != 1 {
		t.Fatalf("near the server certificate's end: changed %v, same CA %v, records %v (%v)", changed, renewed.CACert == ca.CACert, renewed.Installed, err)
	}
	rotated, changed, err := ensureCA(testCluster, m, at.Add(caTTL-serverCertTTL+time.Hour))
	if err != nil || !changed || rotated.CACert == ca.CACert || len(rotated.Installed) != 0 {
		t.Fatalf("near the CA's end: changed %v, same CA %v, records %v (%v)", changed, rotated.CACert == ca.CACert, rotated.Installed, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fresh, changed, err := ensureCA(testCluster, m, at); err != nil || !changed || fresh.CACert == rotated.CACert {
		t.Fatalf("a file that does not read: changed %v (%v)", changed, err)
	}
}

// buildkitObjects are the objects shared Buildkit has on the fake
// connection, with the anchor.
var buildkitObjects = []string{"configmap/anchor", "pvc/buildkit", "configmap/buildkit", "secret/buildkit", "deploy/buildkit", "svc/buildkit"}

func buildkitDeployment(t *testing.T, f *fakePool, m fleetMeta) corev1.PodTemplateSpec {
	t.Helper()
	d, err := f.cs.AppsV1().Deployments("fleet").Get(context.Background(), m.name(names.SuffixBuildkit), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template
}

// TestEnsureBuildkit: the server, its config and its TLS Secret (the
// certificate of the fleet's CA on the daemon host); rootless unless the
// connection allows privileged pods (an explicit setting, or a local
// cluster's auto); its endpoint is its Service's TCP port.
func TestEnsureBuildkit(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	sameList(t, "objects", f.objects(t), want(m, buildkitObjects...))
	ca, err := loadCA(testCluster, m.Hash)
	if err != nil || ca == nil {
		t.Fatalf("no CA on the daemon host: %v", err)
	}
	secret, _ := f.cs.CoreV1().Secrets("fleet").Get(context.Background(), m.name(names.SuffixBuildkit), metav1.GetOptions{})
	if string(secret.Data[tlsCertFile]) != ca.ServerCert || string(secret.Data[tlsCAFile]) != ca.CACert || strings.Contains(string(secret.Data[tlsKeyFile]), ca.CAKey) {
		t.Fatal("the Secret does not hold the server's certificate and CA (or holds the CA's key)")
	}
	tpl := buildkitDeployment(t, f, m)
	if c := tpl.Spec.Containers[0]; c.Image != DefaultBuildkitRootlessImage || tpl.Annotations[annotationBuildkitTLS] != ca.tlsHash() {
		t.Fatalf("rootless by default: image %s annotations %v", c.Image, tpl.Annotations)
	}
	ep, err := rt.Endpoint("alpha", fleet.ServiceBuildkit)
	if err != nil || ep.URL != "tcp://"+m.name(names.SuffixBuildkit)+":1234" || ep.HostPort != m.name(names.SuffixBuildkit)+":1234" {
		t.Fatalf("endpoint %+v %v", ep, err)
	}

	yes := true
	f.rec.AllowPrivileged = &yes
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if c := buildkitDeployment(t, f, m).Spec.Containers[0]; c.Image != DefaultBuildkitImage || !*c.SecurityContext.Privileged {
		t.Fatalf("privileged allowed: image %s", c.Image)
	}
	f.rec.AllowPrivileged = nil
	connectionFacts = func(context.Context, string) (kubernetes.Facts, error) {
		return kubernetes.Facts{Capabilities: &fleetgrpc.KubeCapabilities{LocalCluster: true, BuildkitAdmitted: true}}, nil
	}
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if c := buildkitDeployment(t, f, m).Spec.Containers[0]; c.Image != DefaultBuildkitImage {
		t.Fatalf("a local cluster (privileged auto): image %s", c.Image)
	}
	// No preflight has read the connection yet (the daemon just started):
	// the server keeps the shape it has rather than a guess.
	connectionFacts = func(context.Context, string) (kubernetes.Facts, error) {
		return kubernetes.Facts{}, errors.New("not checked yet")
	}
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if c := buildkitDeployment(t, f, m).Spec.Containers[0]; c.Image != DefaultBuildkitImage || !*c.SecurityContext.Privileged {
		t.Fatalf("facts unknown: the rootful server became image %s", c.Image)
	}
}

// TestBuildkitRefused: a connection whose Allow shared Buildkit is off, or
// whose admission denies the server's pod, refuses it by name (it is
// ErrNoServiceEndpoint, so instances there go without), its ensure removing
// a server left from before (the volume stays), its endpoint and its
// credentials refused too.
func TestBuildkitRefused(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	off := false
	f.rec.AllowSharedBuildkit = &off
	err := rt.EnsureService("alpha", fleet.ServiceBuildkit)
	var refused *BuildkitRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, fleet.ErrNoServiceEndpoint) || !strings.Contains(err.Error(), "allow_shared_buildkit") || !strings.Contains(err.Error(), "minikube") {
		t.Fatalf("not allowed = %v", err)
	}
	sameList(t, "objects of a refused server", f.objects(t), want(m, "configmap/anchor", "pvc/buildkit"))
	if _, err := rt.Endpoint("alpha", fleet.ServiceBuildkit); !errors.As(err, &refused) {
		t.Fatalf("endpoint = %v", err)
	}
	if _, err := rt.IssueCredential("alpha", "i1", fleet.ServiceBuildkit); !errors.As(err, &refused) {
		t.Fatalf("credential = %v", err)
	}

	f.rec.AllowSharedBuildkit = nil
	const reason = "Shared Buildkit unavailable on this cluster: the namespace's admission denies its pod (violates PodSecurity \"baseline:latest\"). Use a sysbox runtime class or a namespace that allows it."
	connectionFacts = func(context.Context, string) (kubernetes.Facts, error) {
		return kubernetes.Facts{Capabilities: &fleetgrpc.KubeCapabilities{BuildkitReason: reason}}, nil
	}
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); !errors.As(err, &refused) || err.Error() != reason {
		t.Fatalf("denied = %v", err)
	}
	if _, err := rt.IssueCredential("alpha", "i1", fleet.ServiceBuildkit); err == nil || err.Error() != reason {
		t.Fatalf("credential where denied = %v", err)
	}
	sameList(t, "objects", f.objects(t), want(m, "configmap/anchor", "pvc/buildkit"))
}

// TestIssueAndRecordCredentials: an instance's certificate (CN its key) is
// recorded once installed, and the records keep only instances on the
// connection; a server whose CA the daemon lost gets a new one with the next
// certificate; a certificate of a CA rotated meanwhile is not recorded, and
// the fleet leaving the connection rotates its CA.
func TestIssueAndRecordCredentials(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.IssueCredential("alpha", "nobody", fleet.ServiceBuildkit); err == nil {
		t.Fatal("an instance with no record on the connection got a certificate")
	}
	if _, err := rt.IssueCredential("alpha", "i1", fleet.ServiceDebCache); err == nil {
		t.Fatal("the deb cache issued a credential")
	}
	cred, err := rt.IssueCredential("alpha", "i1", fleet.ServiceBuildkit)
	if err != nil {
		t.Fatal(err)
	}
	if cert, _ := parseCert(cred.Cert); cert.Subject.CommonName != "key-alpha-i1" {
		t.Fatalf("CN %q", cert.Subject.CommonName)
	}
	if _, _, ok := rt.InstalledCredential("alpha", "i1", fleet.ServiceBuildkit); ok {
		t.Fatal("a certificate is recorded before it is installed")
	}
	if err := rt.CredentialInstalled("alpha", "i1", fleet.ServiceBuildkit, cred); err != nil {
		t.Fatal(err)
	}
	nb, na, ok := rt.InstalledCredential("alpha", "i1", fleet.ServiceBuildkit)
	if !ok || !nb.Equal(cred.NotBefore) || !na.Equal(cred.NotAfter) {
		t.Fatalf("installed %v %v %v", nb, na, ok)
	}
	// i1 leaves the connection: its record goes with the next one written.
	fleetInstanceKeys = func(string, string) (map[string]string, error) { return map[string]string{"i2": "key-alpha-i2"}, nil }
	cred2, err := rt.IssueCredential("alpha", "i2", fleet.ServiceBuildkit)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.CredentialInstalled("alpha", "i2", fleet.ServiceBuildkit, cred2); err != nil {
		t.Fatal(err)
	}
	ca, _ := loadCA(testCluster, m.Hash)
	if len(ca.Installed) != 1 || ca.Installed["key-alpha-i2"].Instance != "i2" {
		t.Fatalf("records %v", ca.Installed)
	}

	// The daemon's CA is lost: the next certificate comes with a new CA,
	// which the server gets first.
	before := buildkitDeployment(t, f, m).Annotations[annotationBuildkitTLS]
	if err := os.Remove(state.KubeBuildkitCAPath(testCluster, m.Hash)); err != nil {
		t.Fatal(err)
	}
	cred3, err := rt.IssueCredential("alpha", "i2", fleet.ServiceBuildkit)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := f.cs.CoreV1().Secrets("fleet").Get(context.Background(), m.name(names.SuffixBuildkit), metav1.GetOptions{})
	if string(cred3.CA) == string(cred2.CA) || string(secret.Data[tlsCAFile]) != string(cred3.CA) || buildkitDeployment(t, f, m).Annotations[annotationBuildkitTLS] == before {
		t.Fatal("a lost CA did not reach the server with the next certificate")
	}
	if err := rt.CredentialInstalled("alpha", "i2", fleet.ServiceBuildkit, cred2); err == nil {
		t.Fatal("a certificate of the earlier CA was recorded")
	}

	fleetUsesCluster = func(string, string) bool { return false }
	if err := rt.ReleaseFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if ca, _ := loadCA(testCluster, m.Hash); ca != nil {
		t.Fatal("the fleet left the connection and its CA is still there")
	}
}

// TestMaintainRenewsTheServersCertificate: the upkeep gives the server a new
// certificate once its one nears its end, which replaces its pod.
func TestMaintainRenewsTheServersCertificate(t *testing.T) {
	f := useFake(t)
	rt := For(testCluster)
	m := meta("alpha")
	if err := rt.EnsureService("alpha", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	before := buildkitDeployment(t, f, m).Annotations[annotationBuildkitTLS]
	if err := rt.Maintain(context.Background(), "alpha", []fleet.SharedService{fleet.ServiceBuildkit}); err != nil {
		t.Fatal(err)
	}
	if got := buildkitDeployment(t, f, m).Annotations[annotationBuildkitTLS]; got != before {
		t.Fatal("the upkeep replaced a server whose certificate is good")
	}
	origNow := now
	t.Cleanup(func() { now = origNow })
	now = func() time.Time { return origNow().Add(serverCertTTL - serverCertRenew + time.Hour) }
	if err := rt.Maintain(context.Background(), "alpha", []fleet.SharedService{fleet.ServiceBuildkit}); err != nil {
		t.Fatal(err)
	}
	if got := buildkitDeployment(t, f, m).Annotations[annotationBuildkitTLS]; got == before {
		t.Fatal("a server certificate near its end was not renewed")
	}

	// The fleet leaves the connection after the upkeep read the state: its
	// rotated CA and its server are not made again.
	fleetUsesCluster = func(string, string) bool { return false }
	if err := rt.ReleaseFleet("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Maintain(context.Background(), "alpha", []fleet.SharedService{fleet.ServiceBuildkit}); err != nil {
		t.Fatal(err)
	}
	if ca, _ := loadCA(testCluster, m.Hash); ca != nil {
		t.Fatal("the upkeep made a CA again for a fleet that left the connection")
	}
	sameList(t, "objects of a fleet that left", f.objects(t), want(m, "pvc/buildkit"))
}

// TestEnsureCAKeepsAGoodServerCertificateWhenTheClockGoesBack: a clock set
// back before the server's certificate began does not replace it (and the
// server's pod with it).
func TestEnsureCAKeepsAGoodServerCertificateWhenTheClockGoesBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := meta("alpha")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ca, _, err := ensureCA(testCluster, m, at)
	if err != nil {
		t.Fatal(err)
	}
	again, changed, err := ensureCA(testCluster, m, at.Add(-6*24*time.Hour))
	if err != nil || changed || again.ServerCert != ca.ServerCert {
		t.Fatalf("a clock 6 days back: changed %v (%v)", changed, err)
	}
	// One for names the server no longer has (another namespace) is, the
	// CA kept.
	moved := m
	moved.Namespace = "elsewhere"
	renamed, changed, err := ensureCA(testCluster, moved, at)
	if err != nil || !changed || renamed.CACert != ca.CACert || renamed.ServerCert == ca.ServerCert {
		t.Fatalf("another namespace: changed %v, same CA %v (%v)", changed, renamed.CACert == ca.CACert, err)
	}
}
