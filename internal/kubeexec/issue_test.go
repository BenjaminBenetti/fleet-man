//go:build !nok8s

package kubeexec

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// countingPlugin writes an exec plugin that appends a line to a counter file
// on every run, waits delay, and prints out.
func countingPlugin(t *testing.T, dir, out string, delay time.Duration) (count func() int) {
	t.Helper()
	counter := filepath.Join(dir, "runs")
	writeFile(t, filepath.Join(dir, "out.json"), out, 0o600)
	script := fmt.Sprintf("#!/bin/sh\necho run >> %q\nsleep %s\ncat %q\n", counter, fmt.Sprintf("%.3f", delay.Seconds()), filepath.Join(dir, "out.json"))
	writeFile(t, filepath.Join(dir, "fake-plugin"), script, 0o755)
	return func() int {
		data, _ := os.ReadFile(counter)
		return bytes.Count(data, []byte("run\n"))
	}
}

func loadIssuer(t *testing.T, kubeconfigYAML string, extraPath ...string) *Issuer {
	t.Helper()
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfigYAML, 0o600)
	conn, err := Load(fleet.KubeCluster{ID: "c-abc234", Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: extraPath},
		Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return NewIssuer(conn)
}

func TestIssueStaticCredentials(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	certPEM, keyPEM := selfSigned(t, expiry)
	certFile := writeFile(t, filepath.Join(dir, "client.crt"), string(certPEM), 0o600)
	keyFile := writeFile(t, filepath.Join(dir, "client.key"), string(keyPEM), 0o600)
	tokenFile := writeFile(t, filepath.Join(dir, "token"), jwt(`{"exp":1800000000}`)+"\n", 0o600)
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	b64 := base64.StdEncoding.EncodeToString

	tests := []struct {
		name string
		user string
		want fleet.KubeExecCredential
	}{
		{"cert data", "    client-certificate-data: " + b64(certPEM) + "\n    client-key-data: " + b64(keyPEM) + "\n",
			fleet.KubeExecCredential{Auth: string(AuthClientCert), ClientCertificateData: certPEM, ClientKeyData: keyPEM, ExpiresAt: expiry}},
		{"cert files", "    client-certificate: " + certFile + "\n    client-key: " + keyFile + "\n",
			fleet.KubeExecCredential{Auth: string(AuthClientCert), ClientCertificateData: certPEM, ClientKeyData: keyPEM, ExpiresAt: expiry}},
		{"token", "    token: " + jwt(`{"exp":1900000000}`) + "\n",
			fleet.KubeExecCredential{Auth: string(AuthToken), BearerToken: jwt(`{"exp":1900000000}`), ExpiresAt: time.Unix(1900000000, 0)}},
		{"token file", "    tokenFile: " + tokenFile + "\n",
			fleet.KubeExecCredential{Auth: string(AuthTokenFile), BearerToken: jwt(`{"exp":1800000000}`), ExpiresAt: time.Unix(1800000000, 0)}},
		{"basic", "    username: admin\n    password: pw\n",
			fleet.KubeExecCredential{Auth: string(AuthBasic), Username: "admin", Password: "pw"}},
		{"impersonation", "    token: opaque\n    as: alice\n    as-uid: uid-7\n    as-groups: [devs, ops]\n    as-user-extra:\n      scopes: [read, write]\n",
			fleet.KubeExecCredential{Auth: string(AuthToken), BearerToken: "opaque", Impersonate: fleet.KubeImpersonation{
				User: "alice", UID: "uid-7", Groups: []string{"devs", "ops"}, Extra: map[string][]string{"scopes": {"read", "write"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := loadIssuer(t, kubeconfig{server: "https://x.example:6443", caData: b64(caPEM), user: tt.user}.yaml())
			got, err := issuer.Issue(context.Background(), false)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			want := tt.want
			want.ClusterID, want.Server, want.CertificateAuthorityData = "c-abc234", "https://x.example:6443", caPEM
			if !got.ExpiresAt.Equal(want.ExpiresAt) {
				t.Errorf("expires %v, want %v", got.ExpiresAt, want.ExpiresAt)
			}
			got.ExpiresAt, want.ExpiresAt = time.Time{}, time.Time{}
			if want.Impersonate.Groups == nil {
				want.Impersonate.Groups = got.Impersonate.Groups
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("issued\n %+v\nwant\n %+v", got, want)
			}
		})
	}

	// Files are read on every issue, so a rotated one is what goes out.
	issuer := loadIssuer(t, kubeconfig{server: "https://x.example:6443", user: "    tokenFile: " + tokenFile + "\n"}.yaml())
	writeFile(t, tokenFile, "rotated\n", 0o600)
	if got, err := issuer.Issue(context.Background(), false); err != nil || got.BearerToken != "rotated" {
		t.Fatalf("after rotation: %q %v", got.BearerToken, err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(context.Background(), false); err == nil {
		t.Fatal("a missing token file issued a credential")
	}
}

const plugin1h = `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"tok-123","expirationTimestamp":"2030-01-02T03:04:05Z"}}`

func pluginKubeconfig(server string) string {
	return kubeconfig{server: server, user: `    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: fake-plugin
      interactiveMode: Never
`}.yaml()
}

func TestIssuePluginCredentialIsCached(t *testing.T) {
	dir := t.TempDir()
	runs := countingPlugin(t, dir, plugin1h, 0)
	issuer := loadIssuer(t, pluginKubeconfig("https://x.example"), dir)
	expiry := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	now := expiry.Add(-time.Hour)
	issuer.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		got, err := issuer.Issue(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.BearerToken != "tok-123" || got.Auth != string(AuthExecPlugin) || !got.ExpiresAt.Equal(expiry) {
			t.Fatalf("issued %+v", got)
		}
	}
	if n := runs(); n != 1 {
		t.Fatalf("the plugin ran %d times for three issues, want once", n)
	}
	if _, err := issuer.Issue(ctx, true); err != nil || runs() != 2 {
		t.Fatalf("refresh: %v, runs %d, want a second run", err, runs())
	}
	// Close to its expiry the credential is minted again on each issue, once
	// per issue (not in a loop, though the new one is no fresher).
	now = expiry.Add(-30 * time.Second)
	for i := range 2 {
		if _, err := issuer.Issue(ctx, false); err != nil {
			t.Fatal(err)
		}
		if n := runs(); n != 3+i {
			t.Fatalf("near expiry: %d runs, want %d", n, 3+i)
		}
	}
}

func TestIssuePluginCredentialWithoutExpiryIsReusedForAWhile(t *testing.T) {
	dir := t.TempDir()
	runs := countingPlugin(t, dir, `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"forever"}}`, 0)
	issuer := loadIssuer(t, pluginKubeconfig("https://x.example"), dir)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return now }
	for range 2 {
		if got, err := issuer.Issue(context.Background(), false); err != nil || got.BearerToken != "forever" || !got.ExpiresAt.IsZero() {
			t.Fatalf("issued %+v %v", got, err)
		}
	}
	now = now.Add(issueMaxAge)
	if _, err := issuer.Issue(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := runs(); n != 2 {
		t.Fatalf("%d runs, want a second after issueMaxAge", n)
	}
}

func TestIssuePluginRunsOnceForConcurrentIssues(t *testing.T) {
	dir := t.TempDir()
	runs := countingPlugin(t, dir, plugin1h, 300*time.Millisecond)
	issuer := loadIssuer(t, pluginKubeconfig("https://x.example"), dir)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := issuer.Issue(context.Background(), false)
			if err == nil && got.BearerToken != "tok-123" {
				err = fmt.Errorf("token %q", got.BearerToken)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := runs(); n != 1 {
		t.Fatalf("8 concurrent issues ran the plugin %d times, want once", n)
	}

	// A caller that gives up does not fail the run the others wait on.
	if _, err := issuer.Issue(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := issuer.Issue(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an impatient caller got %v", err)
	}
}

func TestIssuePluginFailureIsNotCached(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fake-plugin"), "#!/bin/sh\necho 'login expired' >&2\nexit 3\n", 0o755)
	issuer := loadIssuer(t, pluginKubeconfig("https://x.example"), dir)
	_, err := issuer.Issue(context.Background(), false)
	var perr *PluginError
	if !errors.As(err, &perr) || perr.ExitCode != 3 || perr.Stderr != "login expired" {
		t.Fatalf("err %v, want the plugin's failure", err)
	}
	writeFile(t, filepath.Join(dir, "fake-plugin"), "#!/bin/sh\nprintf '%s' '"+plugin1h+"'\n", 0o755)
	if got, err := issuer.Issue(context.Background(), false); err != nil || got.BearerToken != "tok-123" {
		t.Fatalf("after a fixed login: %+v %v", got, err)
	}
}

func TestIssuePluginClientCertificate(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSigned(t, time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	out := fmt.Sprintf(`{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"clientCertificateData":%q,"clientKeyData":%q}}`, certPEM, keyPEM)
	countingPlugin(t, dir, out, 0)
	issuer := loadIssuer(t, pluginKubeconfig("https://x.example"), dir)
	got, err := issuer.Issue(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ClientCertificateData, certPEM) || !bytes.Equal(got.ClientKeyData, keyPEM) || got.BearerToken != "" || got.ExpiresAt.IsZero() {
		t.Fatalf("issued %+v", got)
	}
}

// mtlsServer is an API server that wants a client certificate signed by
// clientCA, and records the impersonation headers it was sent.
func mtlsServer(t *testing.T, clientCA []byte, seen *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.0"}`))
	}))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(clientCA)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// connectProxy is an HTTP CONNECT proxy that counts the tunnels it opens.
func connectProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tunnels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tunnels.Add(1)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(dst, rw); _ = dst.Close() }()
		go func() { _, _ = io.Copy(conn, dst); _ = conn.Close() }()
	}))
	t.Cleanup(srv.Close)
	return srv, &tunnels
}

// The issued transport is the one fleetd's own client uses: a cert-auth
// context (minikube, kind, kubeadm) with impersonation and a kubeconfig
// proxy-url dials the server through the same proxy, with the same client
// certificate and the same impersonation.
func TestIssuedTransportDialsLikeFleetd(t *testing.T) {
	certPEM, keyPEM := selfSigned(t, time.Now().Add(30*time.Minute))
	var seen http.Header
	srv := mtlsServer(t, certPEM, &seen)
	proxy, tunnels := connectProxy(t)
	b64 := base64.StdEncoding.EncodeToString
	ca := b64(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	cfgYAML := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
    proxy-url: %s
contexts:
- name: ctx
  context: {cluster: c, user: u}
users:
- name: u
  user:
    client-certificate-data: %s
    client-key-data: %s
    as: alice
    as-groups: [devs]
`, srv.URL, ca, proxy.URL, b64(certPEM), b64(keyPEM))
	issuer := loadIssuer(t, cfgYAML)
	cred, err := issuer.Issue(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ProxyURL != proxy.URL {
		t.Fatalf("proxy %q, want the kubeconfig's %q", cred.ProxyURL, proxy.URL)
	}
	cfg, err := RESTConfigFor(cred)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := cs.Discovery().ServerVersion(); err != nil || v.GitVersion != "v1.35.0" {
		t.Fatalf("version through the issued transport: %+v %v", v, err)
	}
	if tunnels.Load() == 0 {
		t.Fatal("the issued transport did not go through the kubeconfig's proxy")
	}
	if seen.Get("Impersonate-User") != "alice" || seen.Get("Impersonate-Group") != "devs" {
		t.Fatalf("impersonation headers %v", seen)
	}

	// Without a proxy the issued transport goes direct, whatever the
	// caller's own environment says.
	cred.ProxyURL = ""
	cfg, err = RESTConfigFor(cred)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := cfg.Proxy(&http.Request{URL: mustURL(t, srv.URL)}); u != nil || err != nil {
		t.Fatalf("direct transport proxies through %v (%v)", u, err)
	}
	before := tunnels.Load()
	cs, _ = kubernetes.NewForConfig(cfg)
	if _, err := cs.Discovery().ServerVersion(); err != nil || tunnels.Load() != before {
		t.Fatalf("direct: %v, tunnels %d -> %d", err, before, tunnels.Load())
	}
}

// A token context authenticates the issued transport with the bearer token.
func TestIssuedTransportCarriesTheToken(t *testing.T) {
	srv, ca := fakeAPIServer(t, "tok-123")
	dir := t.TempDir()
	countingPlugin(t, dir, plugin1h, 0)
	issuer := loadIssuer(t, kubeconfig{server: srv.URL, caData: ca, user: execUser}.yaml(), dir)
	cred, err := issuer.Issue(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := RESTConfigFor(cred)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := kubernetes.NewForConfig(cfg)
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		t.Fatalf("version with the issued token: %v", err)
	}
	cred.BearerToken = "wrong"
	cfg, _ = RESTConfigFor(cred)
	cs, _ = kubernetes.NewForConfig(cfg)
	if _, err := cs.Discovery().ServerVersion(); err == nil {
		t.Fatal("a wrong token was accepted: the test server checks nothing")
	}
}

func TestRESTConfigForRefusesNoServer(t *testing.T) {
	if _, err := RESTConfigFor(fleet.KubeExecCredential{}); err == nil {
		t.Fatal("a credential without a server built a config")
	}
	if _, err := RESTConfigFor(fleet.KubeExecCredential{Server: "https://x", ProxyURL: "::bad"}); err == nil {
		t.Fatal("a bad proxy URL built a config")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
