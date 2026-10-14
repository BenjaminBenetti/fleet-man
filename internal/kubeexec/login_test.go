//go:build !nok8s

package kubeexec

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// A login run relays the device code the plugin prints and nothing else of
// its output: not the credential on stdout, not its other stderr lines. The
// request it gets is interactive.
func TestLoginRelaysOnlyTheDeviceCode(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	info := filepath.Join(dir, "exec-info")
	writeFile(t, filepath.Join(dir, "fake-plugin"), `#!/bin/sh
printf '%s' "$KUBERNETES_EXEC_INFO" > `+info+`
echo "refresh token rt-SECRET-456 is stale" >&2
echo "To sign in, use a web browser to open the page https://microsoft.com/devicelogin and enter the code ABCD1234X to authenticate." >&2
printf '%s' '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"tok-SECRET-123","expirationTimestamp":"2030-01-02T03:04:05Z"}}'
`, 0o755)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	var codes []DeviceCode
	cred, err := conn.Login(context.Background(), func(c DeviceCode) { codes = append(codes, c) })
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !cred.ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) || cred.Auth != AuthExecPlugin {
		t.Errorf("credential = %+v", cred)
	}
	want := DeviceCode{URL: "https://microsoft.com/devicelogin", Code: "ABCD1234X"}
	if len(codes) != 1 || codes[0] != want {
		t.Fatalf("relayed %+v, want only %+v", codes, want)
	}
	data, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"interactive":true`) {
		t.Errorf("the login request is not interactive: %s", data)
	}
}

// LoginCommand runs the plugin as a login in the caller's terminal: the
// request is interactive, stdin and stderr are the caller's to connect, and
// the credential on stdout goes nowhere. A connection without a plugin has
// none to run.
func TestLoginCommand(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	info := filepath.Join(dir, "exec-info")
	writeFile(t, filepath.Join(dir, "fake-plugin"), `#!/bin/sh
printf '%s' "$KUBERNETES_EXEC_INFO" > `+info+`
read answer
echo "signed in as $answer" >&2
printf '%s' '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"tok-SECRET-123"}}'
`, 0o755)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}},
		Options{Environ: []string{"PATH=/usr/bin:/bin", "DAEMON_SECRET=s3"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := conn.LoginCommand()
	if err != nil {
		t.Fatalf("LoginCommand: %v", err)
	}
	if cmd.Stdin != nil || cmd.Stderr != nil {
		t.Fatal("stdin and stderr must be left for the caller's terminal")
	}
	if cmd.Stdout != io.Discard {
		t.Fatalf("stdout = %v: the credential must go nowhere", cmd.Stdout)
	}
	for _, e := range cmd.Env {
		if e == "DAEMON_SECRET=s3" {
			t.Fatal("the caller's other variables must reach the plugin blanked, as fleetd's do")
		}
	}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = strings.NewReader("ben\n"), &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stderr.String() != "signed in as ben\n" {
		t.Errorf("stderr = %q: the plugin must talk to the terminal", stderr.String())
	}
	data, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"interactive":true`) {
		t.Errorf("the login request is not interactive: %s", data)
	}

	static := writeFile(t, filepath.Join(dir, "static"), kubeconfig{server: "https://x.example", user: "    token: t\n"}.yaml(), 0o600)
	conn, err = Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{static}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.LoginCommand(); !errors.Is(err, ErrNoLoginPlugin) {
		t.Fatalf("LoginCommand on a token context = %v, want ErrNoLoginPlugin", err)
	}
}

// A failed login reports the plugin's sanitized stderr, and a connection
// without a plugin has nothing to log in with.
func TestLoginFailures(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	writeFile(t, filepath.Join(dir, "fake-plugin"), "#!/bin/sh\nprintf 'login \\033[1mrefused\\033[0m by the IdP\\n' >&2\nexit 4\n", 0o755)
	conn, err := Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{path}, ExtraPath: []string{dir}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Login(context.Background(), nil)
	var perr *PluginError
	if !errors.As(err, &perr) || perr.ExitCode != 4 || perr.Stderr != "login refused by the IdP" {
		t.Fatalf("Login = %v (%+v), want exit 4 with the sanitized stderr", err, perr)
	}

	static := writeFile(t, filepath.Join(dir, "static"), kubeconfig{server: "https://x.example", user: "    token: t\n"}.yaml(), 0o600)
	conn, err = Load(fleet.KubeCluster{Context: "ctx", KubeconfigPaths: []string{static}}, Options{Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Login(context.Background(), nil); !errors.Is(err, ErrNoLoginPlugin) {
		t.Fatalf("Login on a token context = %v, want ErrNoLoginPlugin", err)
	}
}

func TestDeviceCodeParser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  DeviceCode
	}{
		{"azure kubelogin", []string{
			"To sign in, use a web browser to open the page https://microsoft.com/devicelogin and enter the code ABCD1234X to authenticate.",
		}, DeviceCode{URL: "https://microsoft.com/devicelogin", Code: "ABCD1234X"}},
		{"aws sso over several lines", []string{
			"Attempting to automatically open the SSO authorization page in your default browser.",
			"If the browser does not open or you wish to use a different device to authorize this request, open the following URL:",
			"",
			"https://device.sso.eu-west-1.amazonaws.com/",
			"",
			"Then enter the code:",
			"",
			"WXYZ-PQRS",
		}, DeviceCode{URL: "https://device.sso.eu-west-1.amazonaws.com/", Code: "WXYZ-PQRS"}},
		{"user code with a complete URI", []string{
			"Open https://login.example.com/activate?user_code=QWER-TYUI, or enter user code: QWER-TYUI.",
		}, DeviceCode{URL: "https://login.example.com/activate?user_code=QWER-TYUI", Code: "QWER-TYUI"}},
		{"words after code are not codes", []string{
			"exchanging the code to authenticate",
			"error: token refresh failed",
		}, DeviceCode{}},
		{"escape sequences are dropped", []string{
			"\x1b[1mOpen\x1b[0m \x1b]8;;https://evil.example\x07https://good.example/device\x1b]8;;\x07 code: 9F3K-22LM",
		}, DeviceCode{URL: "https://good.example/device", Code: "9F3K-22LM"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p DeviceCodeParser
			var got DeviceCode
			for _, line := range tc.lines {
				got, _ = p.Line(line)
			}
			if got != tc.want {
				t.Errorf("parsed %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCurrentContextAndLookupPlugin(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "config"), kubeconfig{server: "https://x.example", user: execUser}.yaml(), 0o600)
	if got, err := CurrentContext([]string{path}); err != nil || got != "other" {
		t.Errorf("CurrentContext = %q, %v; want other", got, err)
	}
	if got, err := CurrentContext([]string{filepath.Join(dir, "missing")}); err != nil || got != "" {
		t.Errorf("CurrentContext of no file = %q, %v", got, err)
	}

	bin := filepath.Join(dir, "bin")
	plugin := writePlugin(t, bin, "fake-plugin", pluginOutput)
	if got, err := LookupPlugin("fake-plugin", []string{bin}, []string{"PATH=/usr/bin"}); err != nil || got != plugin {
		t.Errorf("LookupPlugin via extra path = %q, %v", got, err)
	}
	if got, err := LookupPlugin("fake-plugin", nil, []string{"PATH=" + bin}); err != nil || got != plugin {
		t.Errorf("LookupPlugin via PATH = %q, %v", got, err)
	}
	if _, err := LookupPlugin("fake-plugin", nil, []string{"PATH=/usr/bin"}); err == nil || !strings.Contains(err.Error(), "not on the daemon host's PATH") {
		t.Errorf("LookupPlugin of a missing plugin = %v", err)
	}
}
