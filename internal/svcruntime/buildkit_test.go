package svcruntime_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
)

// buildkitInstance is a stub instance with a stand-in docker that has
// buildx and records its arguments in $HOME/docker.log; `buildx inspect`
// finds the shared builder once $HOME/has-builder exists.
func buildkitInstance(t *testing.T) (*backendtest.Stub, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	s := backendtest.NewStub(t)
	ws := t.TempDir()
	res, err := s.Up(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Container(res.ContainerID)
	bin := filepath.Join(filepath.Dir(c.Home), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	docker := `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/docker.log"
[ "$2" = inspect ] && { [ -e "$HOME/has-builder" ]; exit $?; }
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	return s, ws, c.Home
}

func dockerCalls(t *testing.T, home string) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(home, "docker.log"))
	_ = os.Remove(filepath.Join(home, "docker.log"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" && l != "buildx version" {
			out = append(out, l)
		}
	}
	return out
}

func credentialFiles(home string) string {
	var got []string
	for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
		data, err := os.ReadFile(filepath.Join(home, ".fleet", "buildkit", f))
		if err != nil {
			data = []byte("-\n")
		}
		got = append(got, strings.TrimSpace(string(data)))
	}
	return strings.Join(got, " ")
}

// TestConfigureBuildkitWithACredential: on a runtime that authenticates its
// instances, a create installs the instance's own credential, records it, and
// makes the TLS builder the default; a start keeps an existing builder (and
// still renews the credential); a renewal only renews it.
func TestConfigureBuildkitWithACredential(t *testing.T) {
	s, ws, home := buildkitInstance(t)
	rt := svcruntimetest.NewIssuer("kubernetes:c1")

	configured, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "i1", ws, svcruntime.BuildkitCreate)
	if err != nil || !configured {
		t.Fatalf("create: configured %v, %v", configured, err)
	}
	if got := credentialFiles(home); got != "ca cert key" {
		t.Fatalf("credential in the instance: %q", got)
	}
	if _, notAfter, ok := rt.InstalledCredential("alpha", "i1", "buildkit"); !ok || !notAfter.Equal(rt.Credential.NotAfter) {
		t.Fatalf("installed credential %v %v", notAfter, ok)
	}
	calls := dockerCalls(t, home)
	d := home + "/.fleet/buildkit"
	create := "buildx create --name fleet-shared --driver remote --driver-opt cacert=" + d + "/ca.pem,cert=" + d + "/cert.pem,key=" + d + "/key.pem,servername=kubernetes-c1-buildkit tcp://kubernetes-c1-buildkit:1234"
	if strings.Join(calls, "\n") != "buildx rm fleet-shared\n"+create+"\nbuildx use fleet-shared" {
		t.Fatalf("docker calls on create:\n%s", strings.Join(calls, "\n"))
	}

	if err := os.WriteFile(filepath.Join(home, "has-builder"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rt.Credential.Cert = []byte("renewed\n")
	if configured, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "i1", ws, svcruntime.BuildkitStart); err != nil || !configured {
		t.Fatalf("start: configured %v, %v", configured, err)
	}
	if calls := dockerCalls(t, home); strings.Join(calls, " ") != "buildx inspect fleet-shared" {
		t.Fatalf("a start with the builder there: %v", calls)
	}
	if got := credentialFiles(home); got != "ca renewed key" {
		t.Fatalf("credential after a start: %q", got)
	}

	rt.Credential.Cert = []byte("daily\n")
	if configured, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "i1", ws, svcruntime.BuildkitRenew); err != nil || configured {
		t.Fatalf("renew: configured %v, %v", configured, err)
	}
	if calls := dockerCalls(t, home); len(calls) != 0 {
		t.Fatalf("a renewal ran docker: %v", calls)
	}
	if got := credentialFiles(home); got != "ca daily key" {
		t.Fatalf("credential after a renewal: %q", got)
	}
	var methods []string
	for _, c := range rt.Calls() {
		methods = append(methods, c.Method+":"+c.Instance)
	}
	if want := strings.TrimSpace(strings.Repeat("IssueCredential:i1 CredentialInstalled:i1 ", 3)); strings.Join(methods, " ") != want {
		t.Fatalf("runtime calls %v, want %s", methods, want)
	}
}

// TestConfigureBuildkitThroughTheSocket: the docker runtime's server takes
// no credential: the builder is the socket's, and nothing goes in the
// instance's credential folder.
func TestConfigureBuildkitThroughTheSocket(t *testing.T) {
	s, ws, home := buildkitInstance(t)
	if configured, err := svcruntime.ConfigureBuildkit(svcruntimetest.New("docker"), s, "alpha", "i1", ws, svcruntime.BuildkitCreate); err != nil || !configured {
		t.Fatalf("configured %v, %v", configured, err)
	}
	calls := dockerCalls(t, home)
	if len(calls) != 3 || calls[1] != "buildx create --name fleet-shared --driver remote unix:///run/fleet-buildkit/buildkitd.sock" {
		t.Fatalf("docker calls %v", calls)
	}
	if got := credentialFiles(home); got != "- - -" {
		t.Fatalf("credential files %q", got)
	}
}

// TestConfigureBuildkitWithoutTheServer: a runtime that does not point the
// fleet's instances at its server does nothing and says why
// (ErrNoEndpoint); one whose credential cannot be had leaves a clone without
// the certificate its source's copy gave it.
func TestConfigureBuildkitWithoutTheServer(t *testing.T) {
	s, ws, home := buildkitInstance(t)
	rt := svcruntimetest.NewIssuer("kubernetes:c1")
	rt.FailOn("Endpoint", fmt.Errorf("%w: shared Buildkit is not allowed here", svcruntime.ErrNoEndpoint))
	if _, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "i1", ws, svcruntime.BuildkitCreate); !errors.Is(err, svcruntime.ErrNoEndpoint) {
		t.Fatalf("refused endpoint = %v", err)
	}
	if calls := rt.Calls(); len(calls) != 0 || len(dockerCalls(t, home)) != 0 {
		t.Fatalf("a refused server was configured: %v", calls)
	}

	rt.FailOn("Endpoint", nil)
	if _, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "src", ws, svcruntime.BuildkitCreate); err != nil {
		t.Fatal(err)
	}
	rt.FailOn("IssueCredential", errors.New("the cluster is not reachable"))
	if _, err := svcruntime.ConfigureBuildkit(rt, s, "alpha", "clone", ws, svcruntime.BuildkitCreate); err == nil {
		t.Fatal("a failed issue configured the builder")
	}
	if got := credentialFiles(home); got != "ca - -" {
		t.Fatalf("a clone kept its source's credential: %q", got)
	}
}
