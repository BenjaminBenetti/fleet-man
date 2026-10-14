package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
)

// buildStamped compiles a dependency-free program for linux/amd64 into out,
// with the version stamped as `make dist` stamps fleet's.
func buildStamped(t *testing.T, out, version string, pod bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go build")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module tiny\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	args := []string{"build", "-o", out, "-ldflags", "-s -w -X " + binstore.VersionSymbol + "=" + version}
	if pod {
		args = append(args, "-tags", "nok8s")
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
}

func TestBinStoreCheck(t *testing.T) {
	dist := t.TempDir()
	daemon := filepath.Join(dist, "fleet-linux-amd64")
	buildStamped(t, daemon, "v1.2.3", false)
	buildStamped(t, filepath.Join(dist, "fleet-pod-linux-amd64"), "v1.2.3", true)
	var out bytes.Buffer
	if err := runBinStore([]string{"-daemon", daemon, "-dir", dist, "-arch", "amd64"}, &out); err != nil {
		t.Fatalf("version-equal builds: %v", err)
	}
	if !strings.Contains(out.String(), "linux/amd64 serves") {
		t.Fatalf("output %q", out.String())
	}

	// Another version beside the daemon, no pod build at all, and a full
	// build where the pod build belongs each fail.
	buildStamped(t, filepath.Join(dist, "fleet-pod-linux-amd64"), "v1.2.4", true)
	if err := runBinStore([]string{"-daemon", daemon, "-dir", dist, "-arch", "amd64"}, &out); err == nil || !strings.Contains(err.Error(), "not the daemon's v1.2.3") {
		t.Fatalf("another version: %v", err)
	}
	if err := runBinStore([]string{"-daemon", daemon, "-dir", dist, "-arch", "arm64"}, &out); err == nil {
		t.Fatal("no arm64 pod build passed")
	}
	buildStamped(t, filepath.Join(dist, "fleet-pod-linux-amd64"), "v1.2.3", false)
	if err := runBinStore([]string{"-daemon", daemon, "-dir", dist, "-arch", "amd64"}, &out); err == nil || !strings.Contains(err.Error(), "not the pod build") {
		t.Fatalf("a full build as the pod build: %v", err)
	}
	if err := runBinStore([]string{"-dir", dist}, &out); err == nil {
		t.Fatal("no -daemon passed")
	}
}
