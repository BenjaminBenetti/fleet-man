package buildkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// fakeExecer satisfies instanceExecer. It records the joined commands it is
// asked to run and returns canned stdout: the probe (detected by the
// "buildx version" substring) yields probeOut; every other command succeeds
// with empty output. Backing each call with a real `printf` keeps the
// *backend.Cmd run methods working exactly as in production.
type fakeExecer struct {
	probeOut string
	calls    []string
}

func (f *fakeExecer) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	joined := strings.Join(command, " ")
	f.calls = append(f.calls, joined)
	out := ""
	if strings.Contains(joined, "buildx version") {
		out = f.probeOut
	}
	return backend.NewCmd(exec.Command("printf", "%s", out), nil)
}

// tlsBuilder is a cluster's shared server as the tests name it.
var tlsBuilder = Builder{Endpoint: "tcp://fs-fleet-man-tv5vpl4q-buildkit:1234", ServerName: "fs-fleet-man-tv5vpl4q-buildkit"}

// testCredential is a credential with recognizable stand-in payloads.
var testCredential = fleet.ServiceCredential{
	CA:         []byte("-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"),
	Cert:       []byte("-----BEGIN CERTIFICATE-----\nCLIENT\n-----END CERTIFICATE-----\n"),
	Key:        []byte("-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----\n"),
	ServerName: tlsBuilder.ServerName,
}

// The golden scripts under testdata are rewritten instead of checked with
// FLEET_BUILDKIT_UPDATE=1; review the diff before committing it.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("FLEET_BUILDKIT_UPDATE") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (record it with FLEET_BUILDKIT_UPDATE=1)", err)
	}
	if got+"\n" != string(want) {
		t.Errorf("%s differs from what the script renders now; if intended, record it with FLEET_BUILDKIT_UPDATE=1 and review the diff\ngot:\n%s", path, got)
	}
}

func TestBuildxPresent(t *testing.T) {
	cases := map[string]bool{
		"PRESENT":            true,
		"PRESENT\n":          true,
		"warning\nPRESENT\n": true,
		"ABSENT":             false,
		"ABSENT\n":           false,
		"":                   false,
	}
	for in, want := range cases {
		if got := buildxPresent(in); got != want {
			t.Errorf("buildxPresent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProbeScript(t *testing.T) {
	probe := probeScript()
	for _, want := range []string{"command -v docker", "docker buildx version", probeMarkerPresent, probeMarkerAbsent} {
		if !strings.Contains(probe, want) {
			t.Errorf("probeScript missing %q: %s", want, probe)
		}
	}
}

// TestConfigureScriptGolden pins the builder scripts: the docker runtime's
// socket builder, a cluster's mutual-TLS builder, and the start's variant
// that keeps an existing builder.
func TestConfigureScriptGolden(t *testing.T) {
	for _, c := range []struct {
		golden  string
		builder Builder
		keep    bool
	}{
		{"configure-socket.sh", SocketBuilder(), false},
		{"configure-tls.sh", tlsBuilder, false},
		{"ensure-tls.sh", tlsBuilder, true},
	} {
		t.Run(c.golden, func(t *testing.T) {
			script, err := configureScript(c.builder, c.keep)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.golden, script)
		})
	}
}

// TestConfigureScriptRefusesWhatBuildxWouldMisread: an endpoint that is not
// a unix:// or tcp:// address, and a server name that is not a DNS name
// (buildx splits its driver options on commas), are refused, not quoted.
func TestConfigureScriptRefusesWhatBuildxWouldMisread(t *testing.T) {
	for _, b := range []Builder{
		{Endpoint: "http://x:1234"},
		{Endpoint: "tcp://x:1234; rm -rf /"},
		{Endpoint: "tcp://x:1234", ServerName: "a,cert=/etc/passwd"},
		{Endpoint: "tcp://x:1234", ServerName: "UPPER"},
	} {
		if s, err := configureScript(b, false); err == nil {
			t.Errorf("%+v rendered %q", b, s)
		}
	}
}

// TestConfigureScriptRunsBuildxWithAbsolutePaths runs the TLS script with a
// stand-in docker that records its arguments: buildx gets the credential
// files by absolute path under the user's home, the server name, and the
// endpoint; with keep, an existing builder stops it before anything else.
func TestConfigureScriptRunsBuildxWithAbsolutePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	log := filepath.Join(dir, "docker.log")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n[ \"$2\" = inspect ] && [ -n \"$FLEET_TEST_HAS_BUILDER\" ] && exit 0\n[ \"$2\" = inspect ] && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(keep bool, env ...string) []string {
		t.Helper()
		_ = os.Remove(log)
		script, err := configureScript(tlsBuilder, keep)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + home}, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script: %v (%s)", err, out)
		}
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	calls := run(false)
	want := "buildx create --name fleet-shared --driver remote --driver-opt cacert=" + home + "/.fleet/buildkit/ca.pem,cert=" + home + "/.fleet/buildkit/cert.pem,key=" + home + "/.fleet/buildkit/key.pem,servername=" + tlsBuilder.ServerName + " " + tlsBuilder.Endpoint
	if len(calls) != 3 || calls[0] != "buildx rm fleet-shared" || calls[1] != want || calls[2] != "buildx use fleet-shared" {
		t.Fatalf("docker calls:\n%s\nwant rm, then\n%s\nthen use", strings.Join(calls, "\n"), want)
	}
	if calls := run(true); len(calls) != 3 || calls[0] != "buildx inspect fleet-shared" || calls[1] != want {
		t.Fatalf("keep without a builder: %v", calls)
	}
	if calls := run(true, "FLEET_TEST_HAS_BUILDER=1"); len(calls) != 1 || calls[0] != "buildx inspect fleet-shared" {
		t.Fatalf("keep with a builder: %v", calls)
	}
}

func TestConfigureInstanceBuildxSkipsWithoutDocker(t *testing.T) {
	fe := &fakeExecer{probeOut: probeMarkerAbsent}
	configured, err := ConfigureInstanceBuildx(fe, "/ws", SocketBuilder())
	if err != nil {
		t.Fatalf("ConfigureInstanceBuildx (absent) = %v, want nil", err)
	}
	if configured {
		t.Fatalf("ConfigureInstanceBuildx (absent) configured = true, want false")
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected only the probe call, got %d: %v", len(fe.calls), fe.calls)
	}
	for _, c := range fe.calls {
		if strings.Contains(c, "buildx create") {
			t.Fatalf("must NOT configure buildx when docker/buildx absent: %v", fe.calls)
		}
	}
}

func TestConfigureInstanceBuildxConfiguresWhenPresent(t *testing.T) {
	for name, configure := range map[string]func(instanceExecer, string, Builder) (bool, error){
		"configure": ConfigureInstanceBuildx,
		"ensure":    EnsureInstanceBuildx,
	} {
		t.Run(name, func(t *testing.T) {
			fe := &fakeExecer{probeOut: probeMarkerPresent}
			configured, err := configure(fe, "/ws", tlsBuilder)
			if err != nil {
				t.Fatalf("(present) = %v, want nil", err)
			}
			if !configured {
				t.Fatalf("(present) configured = false, want true")
			}
			if len(fe.calls) != 2 {
				t.Fatalf("expected probe + configure, got %d: %v", len(fe.calls), fe.calls)
			}
			if !strings.Contains(fe.calls[1], "buildx create --name "+builderName) || !strings.Contains(fe.calls[1], "servername=$n") {
				t.Fatalf("configure call did not create the shared TLS builder: %v", fe.calls[1])
			}
			if keeps := strings.Contains(fe.calls[1], "buildx inspect"); keeps != (name == "ensure") {
				t.Fatalf("%s: keeps an existing builder = %v: %v", name, keeps, fe.calls[1])
			}
		})
	}
}

// TestCredentialScriptGolden pins the credential install.
func TestCredentialScriptGolden(t *testing.T) {
	script, err := credentialScript(testCredential)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "credential.sh", script)
	if _, err := credentialScript(fleet.ServiceCredential{CA: testCredential.CA}); err == nil {
		t.Fatal("an incomplete credential was rendered")
	}
}

// TestCredentialScriptInstalls runs the install twice in a temporary home:
// the files hold the credential, the key and its directory are the user's
// alone, a second install replaces the first, and nothing is left beside
// them.
func TestCredentialScriptInstalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	home := t.TempDir()
	install := func(cred fleet.ServiceCredential) {
		t.Helper()
		script, err := credentialScript(cred)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install: %v (%s)", err, out)
		}
	}
	install(testCredential)
	renewed := testCredential
	renewed.Cert = []byte("-----BEGIN CERTIFICATE-----\nRENEWED\n-----END CERTIFICATE-----\n")
	install(renewed)
	dir := filepath.Join(home, ".fleet", "buildkit")
	for name, want := range map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"ca.pem":   {testCredential.CA, 0o644},
		"cert.pem": {renewed.Cert, 0o644},
		"key.pem":  {testCredential.Key, 0o600},
	} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil || string(data) != string(want.data) {
			t.Fatalf("%s = %q (%v), want %q", name, data, err, want.data)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != want.mode {
			t.Errorf("%s mode %v, want %v", name, st.Mode().Perm(), want.mode)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("credential dir mode %v, want 0700", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("credential dir holds %d entries, want the three files", len(entries))
	}
}
