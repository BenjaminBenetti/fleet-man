//go:build !nok8s

package kubeexec

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// kubeconfig is a test kubeconfig builder.
type kubeconfig struct {
	server, caData string
	user           string // the YAML of the "u" user's body
	extra          string // more contexts
}

func (k kubeconfig) yaml() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: other
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
- name: nowhere
  cluster:
    server: https://nowhere.invalid
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: ns1}
- name: other
  context: {cluster: nowhere, user: u}
%susers:
- name: u
  user:
%s`, k.server, k.caData, k.extra, k.user)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeAPIServer answers /version for a request bearing token, 401 otherwise.
func fakeAPIServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.0"}`))
	}))
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, base64.StdEncoding.EncodeToString(ca)
}

const pluginOutput = `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"tok-123","expirationTimestamp":"2030-01-02T03:04:05Z"}}`

// writePlugin writes an exec plugin that dumps its environment to $DUMP and
// prints out.
func writePlugin(t *testing.T, dir, name, out string) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, name), "#!/bin/sh\nenv > \"$DUMP\"\nprintf '%s' '"+out+"'\n", 0o755)
}

const execUser = `    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: fake-plugin
      args: ["--flag"]
      env:
      - {name: FROM_KUBECONFIG, value: "yes"}
      interactiveMode: IfAvailable
      provideClusterInfo: true
      installHint: "run \u001b[31mbrew install fake-plugin\u001b[0m"
`

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the plugin's env dump: %v", err)
	}
	return envMap(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func TestLoadPinsPathsAndContext(t *testing.T) {
	dir := t.TempDir()
	first := writeFile(t, filepath.Join(dir, "a"), kubeconfig{server: "https://first.example:6443", caData: "", user: "    token: t\n"}.yaml(), 0o600)
	second := writeFile(t, filepath.Join(dir, "b"), kubeconfig{server: "https://second.example:6443", user: "    token: t\n"}.yaml(), 0o600)
	elsewhere := writeFile(t, filepath.Join(dir, "elsewhere"), kubeconfig{server: "https://elsewhere.example", user: "    token: t\n"}.yaml(), 0o600)

	conn, err := Load(fleet.KubeCluster{ID: "c-abc234", Context: "ctx", Namespace: "fleet", KubeconfigPaths: []string{first, second}},
		Options{Environ: []string{"KUBECONFIG=" + elsewhere, "HOME=" + dir}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The record's context in the first file wins: not the file's
	// current-context, not $KUBECONFIG.
	if conn.Server != "https://first.example:6443" || conn.Context != "ctx" || conn.Namespace != "fleet" || conn.Auth != AuthToken {
		t.Errorf("connection = %+v", conn)
	}
	cfg := conn.RESTConfig()
	if cfg.QPS != QPS || cfg.Burst != Burst || !strings.HasPrefix(cfg.UserAgent, "fleet/") {
		t.Errorf("rest config QPS=%v Burst=%v UA=%q", cfg.QPS, cfg.Burst, cfg.UserAgent)
	}
	cfg.Host = "mutated"
	if conn.RESTConfig().Host == "mutated" {
		t.Error("RESTConfig hands out the connection's own config")
	}
}

// Every RESTConfig is a copy of its own, the exec provider included, and
// copies may be taken concurrently (rest.CopyConfig alone shares the exec
// provider and writes through it when the cluster carries the exec
// extension; run with -race).
func TestRESTConfigCopiesTheExecProvider(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "fake-plugin", pluginOutput)
	withExtension := strings.Replace(kubeconfig{server: "https://x.example", user: execUser}.yaml(),
		"    server: https://x.example\n",
		"    server: https://x.example\n    extensions:\n    - name: client.authentication.k8s.io/exec\n      extension: {audience: fleet}\n", 1)
	path := writeFile(t, filepath.Join(dir, "config"), withExtension, 0o600)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}}, Options{Environ: []string{"PATH=/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := conn.RESTConfig(), conn.RESTConfig()
	if a.ExecProvider == nil || a.ExecProvider.Config == nil {
		t.Fatalf("exec provider %+v lacks the cluster's exec extension", a.ExecProvider)
	}
	if a.ExecProvider == b.ExecProvider {
		t.Fatal("two RESTConfig copies share one exec provider")
	}
	a.ExecProvider.Env = nil
	a.ExecProvider.Command = "/elsewhere"
	if c := conn.RESTConfig(); c.ExecProvider.Command != conn.Plugin.Path || len(c.ExecProvider.Env) == 0 {
		t.Errorf("editing a copy changed the connection: %+v", c.ExecProvider)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = conn.RESTConfig()
			_, _ = conn.Clientset()
		}()
	}
	wg.Wait()
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: "    token: t\n"}.yaml(), 0o600)
	noServer := writeFile(t, filepath.Join(dir, "noserver"), strings.Replace(kubeconfig{server: "https://x.example", user: "    token: t\n"}.yaml(), "    server: https://x.example\n", "", 1), 0o600)
	authProvider := writeFile(t, filepath.Join(dir, "authprovider"), kubeconfig{server: "https://x.example", user: "    auth-provider:\n      name: oidc\n"}.yaml(), 0o600)
	missingPlugin := writeFile(t, filepath.Join(dir, "noplugin"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	garbage := writeFile(t, filepath.Join(dir, "garbage"), "{{{ not yaml", 0o600)

	tests := []struct {
		name    string
		record  fleet.KubeCluster
		want    error
		message string
	}{
		{"no context", fleet.KubeCluster{KubeconfigPaths: []string{good}}, ErrMisconfigured, "no kubeconfig context"},
		{"no paths", fleet.KubeCluster{Context: "ctx"}, ErrMisconfigured, "no kubeconfig paths"},
		{"missing files", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{filepath.Join(dir, "nope")}}, ErrKubeconfigMissing, "nope"},
		{"unknown context", fleet.KubeCluster{Context: "renamed", KubeconfigPaths: []string{good}}, ErrContextMissing, "renamed"},
		{"no server", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{noServer}}, ErrMisconfigured, "no server"},
		{"auth provider", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{authProvider}}, ErrMisconfigured, "kubelogin"},
		{"unparsable", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{garbage}}, ErrMisconfigured, "reading kubeconfig"},
		{"bad plugin_env", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{good}, PluginEnv: []string{"MY_TOKEN=x"}}, ErrMisconfigured, "looks like a secret"},
		{"bad extra_path", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{good}, ExtraPath: []string{"bin"}}, ErrMisconfigured, "absolute"},
		{"plugin missing", fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{missingPlugin}}, ErrPluginMissing, "extra_path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.record, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Load = %v, want %v containing %q", err, tt.want, tt.message)
			}
		})
	}
	// The install hint is relayed, with no raw escape sequence left in it
	// (client-go spells them out as text).
	_, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{missingPlugin}}, Options{Environ: []string{"PATH=/usr/bin"}})
	if !strings.Contains(err.Error(), "brew install fake-plugin") || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("plugin-missing error = %q, want the sanitized install hint", err)
	}
}

// A connection never falls back to the in-cluster config, even inside a pod.
func TestLoadNeverInCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	dir := t.TempDir()
	empty := writeFile(t, filepath.Join(dir, "config"), "apiVersion: v1\nkind: Config\n", 0o600)
	if _, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{empty}}, Options{}); !errors.Is(err, ErrContextMissing) {
		t.Fatalf("Load of an empty kubeconfig in a pod = %v, want ErrContextMissing", err)
	}
}

// The plugin runs by its absolute path, found through extra_path, with only
// the base variables, the kubeconfig's env and plugin_env; client-go is
// pinned to that path, never interactive, and every other daemon variable is
// blanked for it.
func TestLoadPinsThePlugin(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "sdk", "bin")
	plugin := writePlugin(t, pluginDir, "fake-plugin", pluginOutput)
	// A plugin of the same name in a relative PATH entry is never picked.
	writePlugin(t, dir, "fake-plugin", "{}")
	t.Chdir(dir)
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	environ := []string{
		"PATH=.:/usr/bin:/bin", "HOME=/home/me", "LANG=C.UTF-8", "LC_TIME=en_GB", "HTTPS_PROXY=http://proxy:3128",
		"AWS_PROFILE=dev", "AWS_SECRET_ACCESS_KEY=pass-me", "GITHUB_TOKEN=never", "FLEET_MCP_TOKEN=never", "UNSET_NAMED=",
	}
	conn, err := Load(fleet.KubeCluster{
		ID: "c-abc234", Context: "ctx", KubeconfigPaths: []string{path},
		ExtraPath: []string{pluginDir},
		PluginEnv: []string{"AWS_PROFILE", "AWS_SECRET_ACCESS_KEY", "AWS_REGION=eu-west-1", "NOT_IN_DAEMON_ENV"},
	}, Options{Environ: environ})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if conn.Auth != AuthExecPlugin || conn.Plugin == nil || conn.Plugin.Path != plugin || conn.Plugin.Command != "fake-plugin" {
		t.Fatalf("connection plugin = %+v", conn.Plugin)
	}
	wantEnv := []string{
		"AWS_PROFILE=dev", "AWS_REGION=eu-west-1", "AWS_SECRET_ACCESS_KEY=pass-me", "FROM_KUBECONFIG=yes",
		"HOME=/home/me", "HTTPS_PROXY=http://proxy:3128", "LANG=C.UTF-8", "LC_TIME=en_GB",
		"PATH=" + pluginDir + ":.:/usr/bin:/bin",
	}
	if got := strings.Join(conn.Plugin.Env, " "); got != strings.Join(wantEnv, " ") {
		t.Errorf("plugin env\n got %s\nwant %s", got, strings.Join(wantEnv, " "))
	}

	ec := conn.RESTConfig().ExecProvider
	if ec.Command != plugin || ec.InteractiveMode != clientcmdapi.NeverExecInteractiveMode || !ec.StdinUnavailable {
		t.Errorf("exec provider = %+v", ec)
	}
	if ec.PluginPolicy.PolicyType != clientcmdapi.PluginPolicyAllowlist || len(ec.PluginPolicy.Allowlist) != 1 || ec.PluginPolicy.Allowlist[0].Name != plugin {
		t.Errorf("plugin policy = %+v", ec.PluginPolicy)
	}
	effective := map[string]string{}
	for _, kv := range environ { // what client-go starts the plugin with
		name, value, _ := strings.Cut(kv, "=")
		effective[name] = value
	}
	for _, e := range ec.Env {
		effective[e.Name] = e.Value
	}
	for name, want := range map[string]string{"GITHUB_TOKEN": "", "FLEET_MCP_TOKEN": "", "UNSET_NAMED": "", "AWS_SECRET_ACCESS_KEY": "pass-me", "FROM_KUBECONFIG": "yes", "PATH": pluginDir + ":.:/usr/bin:/bin"} {
		if effective[name] != want {
			t.Errorf("client-go would run the plugin with %s=%q, want %q", name, effective[name], want)
		}
	}
}

// End to end through client-go: the request carries the plugin's token, and
// the plugin saw no daemon secret it was not given.
func TestClientsetAuthenticatesThroughThePlugin(t *testing.T) {
	srv, ca := fakeAPIServer(t, "tok-123")
	dir := t.TempDir()
	writePlugin(t, dir, "fake-plugin", pluginOutput)
	dump := filepath.Join(dir, "env.dump")
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: srv.URL, caData: ca, user: execUser}.yaml(), 0o600)
	t.Setenv("FLEET_TEST_DAEMON_SECRET_TOKEN", "must-not-leak")

	conn, err := Load(fleet.KubeCluster{ID: "c-abc234", Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}, PluginEnv: []string{"DUMP=" + dump}}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatalf("Clientset: %v", err)
	}
	if again, _ := conn.Clientset(); again != cs {
		t.Error("Clientset built a second client")
	}
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("ServerVersion through the plugin's token: %v", err)
	}
	if v.GitVersion != "v1.35.0" {
		t.Errorf("version = %+v", v)
	}
	env := readEnv(t, dump)
	if env["FLEET_TEST_DAEMON_SECRET_TOKEN"] != "" {
		t.Errorf("a daemon secret reached the plugin: %q", env["FLEET_TEST_DAEMON_SECRET_TOKEN"])
	}
	if env["FROM_KUBECONFIG"] != "yes" || !strings.Contains(env[execInfoEnv], `"interactive":false`) {
		t.Errorf("plugin env FROM_KUBECONFIG=%q %s=%q", env["FROM_KUBECONFIG"], execInfoEnv, env[execInfoEnv])
	}

	// Credential runs the plugin exactly as client-go just did, so what it
	// reads is what the connection authenticates with: the same environment,
	// blanked variables included, and the same request.
	if err := os.Remove(dump); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Credential(context.Background()); err != nil {
		t.Fatalf("Credential: %v", err)
	}
	probe := readEnv(t, dump)
	for name, value := range env {
		if got, ok := probe[name]; !ok || got != value {
			t.Errorf("client-go ran the plugin with %s=%q, Credential with %q (set: %v)", name, value, got, ok)
		}
	}
	for name := range probe {
		if _, ok := env[name]; !ok {
			t.Errorf("Credential ran the plugin with %s, which client-go did not set", name)
		}
	}
}

func TestCredentialFromPlugin(t *testing.T) {
	srv, ca := fakeAPIServer(t, "tok-123")
	dir := t.TempDir()
	writePlugin(t, dir, "fake-plugin", pluginOutput)
	dump := filepath.Join(dir, "env.dump")
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: srv.URL, caData: ca, user: execUser}.yaml(), 0o600)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}, PluginEnv: []string{"DUMP=" + dump}},
		Options{Environ: []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "GITHUB_TOKEN=never"}})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := conn.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.Auth != AuthExecPlugin || !cred.Refreshable || !cred.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("credential = %+v", cred)
	}
	env := readEnv(t, dump)
	if got, ok := env["GITHUB_TOKEN"]; !ok || got != "" {
		t.Errorf("the probe run gave the plugin GITHUB_TOKEN=%q (set: %v), want it blanked as client-go's runs see it", got, ok)
	}
	info := env[execInfoEnv]
	if !strings.Contains(info, `"interactive":false`) || !strings.Contains(info, srv.URL) || !strings.Contains(info, `"apiVersion":"client.authentication.k8s.io/v1"`) {
		t.Errorf("%s = %s, want a non-interactive v1 request with the cluster", execInfoEnv, info)
	}
}

func TestCredentialPluginFailures(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	load := func(script string) *Connection {
		t.Helper()
		writeFile(t, filepath.Join(dir, "fake-plugin"), "#!/bin/sh\n"+script, 0o755)
		conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	long := strings.Repeat("x", 2000)
	_, err := load("printf '\\033[31mtoken expired\\033[0m; run aws sso login\\a\\n" + long + "' >&2; exit 3").Credential(context.Background())
	var perr *PluginError
	if !errors.As(err, &perr) || perr.ExitCode != 3 {
		t.Fatalf("Credential = %v, want a PluginError exit 3", err)
	}
	if !strings.HasPrefix(perr.Stderr, "token expired; run aws sso login\nxxx") || len(perr.Stderr) > MaxPluginMessage || strings.ContainsAny(perr.Stderr, "\x1b\a") {
		t.Errorf("stderr = %q (%d bytes)", perr.Stderr, len(perr.Stderr))
	}

	for name, out := range map[string]string{
		"wrong version": `{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"t"}}`,
		"no status":     `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential"}`,
		"no token":      `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{}}`,
		"not json":      `hello`,
	} {
		if _, err := load("printf '%s' '" + out + "'").Credential(context.Background()); !errors.As(err, &perr) {
			t.Errorf("%s: Credential = %v, want a PluginError", name, err)
		}
	}

	// A hung plugin is bounded by the context, and the children it forked
	// die with it.
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := load("sleep 30 & echo $! > " + pidFile + "\nwait\n").Credential(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a hung plugin = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > pluginWaitDelay {
		t.Errorf("a hung plugin held Credential for %v", elapsed)
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("the plugin did not record its child: %v", err)
		}
		stat := "/proc/" + strings.TrimSpace(string(data)) + "/stat"
		deadline := time.Now().Add(2 * time.Second)
		for {
			b, err := os.ReadFile(stat)
			if err != nil || strings.Contains(string(b), ") Z ") { // gone, or a zombie awaiting its reaper
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the plugin's child outlived the cancel: %s", b)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// selfSigned returns a PEM certificate and key that expire at notAfter.
func selfSigned(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "minikube-user"}, NotBefore: notAfter.Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func jwt(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".sig"
}

func TestCredentialStatic(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	certPEM, keyPEM := selfSigned(t, expiry)
	certFile := writeFile(t, filepath.Join(dir, "client.crt"), string(certPEM), 0o600)
	keyFile := writeFile(t, filepath.Join(dir, "client.key"), string(keyPEM), 0o600)
	tokenFile := writeFile(t, filepath.Join(dir, "token"), jwt(`{"exp":1800000000}`)+"\n", 0o600)
	b64 := base64.StdEncoding.EncodeToString

	tests := []struct {
		name        string
		user        string
		auth        AuthKind
		expires     time.Time
		refreshable bool
	}{
		{"cert data", "    client-certificate-data: " + b64(certPEM) + "\n    client-key-data: " + b64(keyPEM) + "\n", AuthClientCert, expiry, false},
		{"cert files", "    client-certificate: " + certFile + "\n    client-key: " + keyFile + "\n", AuthClientCert, expiry, false},
		{"jwt", "    token: " + jwt(`{"exp":1900000000,"sub":"x"}`) + "\n", AuthToken, time.Unix(1900000000, 0), false},
		{"opaque token", "    token: abcdef.0123456789abcdef\n", AuthToken, time.Time{}, false},
		{"token file", "    tokenFile: " + tokenFile + "\n", AuthTokenFile, time.Unix(1800000000, 0), true},
		{"basic", "    username: admin\n    password: pw\n", AuthBasic, time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml"), kubeconfig{server: "https://x.example", user: tt.user}.yaml(), 0o600)
			conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}}, Options{Environ: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			cred, err := conn.Credential(context.Background())
			if err != nil {
				t.Fatalf("Credential: %v", err)
			}
			if cred.Auth != tt.auth || !cred.ExpiresAt.Equal(tt.expires) || cred.Refreshable != tt.refreshable {
				t.Errorf("credential = %+v, want %s expiring %v (refreshable %v)", cred, tt.auth, tt.expires, tt.refreshable)
			}
		})
	}
}

// A plugin that returns a client certificate expires no later than the
// certificate does.
func TestCredentialPluginCertificate(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSigned(t, time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	out := fmt.Sprintf(`{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"clientCertificateData":%q,"clientKeyData":%q,"expirationTimestamp":"2030-01-01T00:00:00Z"}}`, certPEM, keyPEM)
	writeFile(t, filepath.Join(dir, "out.json"), out, 0o600)
	writeFile(t, filepath.Join(dir, "fake-plugin"), "#!/bin/sh\ncat \""+filepath.Join(dir, "out.json")+"\"\n", 0o755)
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := conn.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !cred.ExpiresAt.Equal(time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expiry = %v, want the certificate's NotAfter", cred.ExpiresAt)
	}
}

// A bare plugin name is looked up only in absolute PATH entries.
func TestResolvePluginSkipsRelativeEntries(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "fake-plugin", "{}")
	t.Chdir(dir)
	if got, err := resolvePlugin("fake-plugin", ".:bin:"+filepath.Join(dir, "missing")); err == nil {
		t.Fatalf("resolvePlugin found %s through a relative PATH entry", got)
	}
	if got, err := resolvePlugin("fake-plugin", ".:"+dir); err != nil || got != filepath.Join(dir, "fake-plugin") {
		t.Fatalf("resolvePlugin = %q, %v", got, err)
	}
	if _, err := resolvePlugin("./fake-plugin", dir); err == nil {
		t.Error("a relative plugin path resolved")
	}
	writeFile(t, filepath.Join(dir, "not-exec"), "#!/bin/sh\n", 0o644)
	if _, err := resolvePlugin(filepath.Join(dir, "not-exec"), ""); err == nil {
		t.Error("a non-executable plugin resolved")
	}
}

func TestSanitizeOutput(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                  "plain",
		"  padded\n\n":                           "padded",
		"\x1b[1;31mred\x1b[0m text":              "red text",
		"osc \x1b]8;;https://x.example\x07link":  "osc link",
		"osc-st \x1b]0;title\x1b\\done":          "osc-st done",
		"bell\a and \x00 nul \r cr":              "bell and  nul  cr",
		"keep\ttabs\nand lines":                  "keep\ttabs\nand lines",
		"bad utf8 \xff":                          "bad utf8 \uFFFD",
		"c1 \u0085 control":                      "c1  control",
		"open the URL https://microsoft.com/dev": "open the URL https://microsoft.com/dev",
	} {
		if got := SanitizeOutput([]byte(in)); got != want {
			t.Errorf("SanitizeOutput(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeOutput([]byte(strings.Repeat("é", 1000)))
	if len(long) > MaxPluginMessage || !strings.HasSuffix(long, "…") || !strings.HasPrefix(long, "éé") {
		t.Errorf("long output = %d bytes, %q...", len(long), long[:10])
	}
}

func TestDefaultKubeconfigPaths(t *testing.T) {
	got := DefaultKubeconfigPaths([]string{"KUBECONFIG=/a:/b::/a:/c"})
	if strings.Join(got, ",") != "/a,/b,/c" {
		t.Errorf("DefaultKubeconfigPaths = %v", got)
	}
	got = DefaultKubeconfigPaths(nil)
	if len(got) != 1 || filepath.Base(got[0]) != "config" || filepath.Base(filepath.Dir(got[0])) != ".kube" {
		t.Errorf("default = %v, want ~/.kube/config", got)
	}
}

func TestListContexts(t *testing.T) {
	dir := t.TempDir()
	first := writeFile(t, filepath.Join(dir, "a"), kubeconfig{server: "https://first.example", user: execUser}.yaml(), 0o600)
	second := writeFile(t, filepath.Join(dir, "b"), kubeconfig{server: "https://second.example", user: "    token: t\n",
		extra: "- name: only-b\n  context: {cluster: c, user: u, namespace: team}\n"}.yaml(), 0o600)
	got, err := ListContexts([]string{first, second, filepath.Join(dir, "missing")})
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	byName := map[string]KubeContext{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if len(got) != 3 {
		t.Fatalf("contexts = %+v, want ctx, other, only-b", got)
	}
	if c := byName["ctx"]; c.Source != first || c.Server != "https://first.example" || c.Namespace != "ns1" || c.Auth != AuthExecPlugin || c.Plugin != "fake-plugin" {
		t.Errorf("ctx = %+v", c)
	}
	// only-b comes from the second file, but resolves through the merged
	// kubeconfig, where the first file's cluster and user win.
	if c := byName["only-b"]; c.Source != second || c.Namespace != "team" || c.Server != "https://first.example" {
		t.Errorf("only-b = %+v", c)
	}
}
