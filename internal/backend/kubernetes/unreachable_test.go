//go:build !nok8s

package kubernetes

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// closedServer is the URL of a loopback port nothing listens on: an API
// server that is down.
func closedServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "https://" + addr
}

// newUnreachableRig is the instance web on a connection whose API server
// does not answer, through a real client-go clientset (nothing stands in for
// the dial), with its instance record saved.
func newUnreachableRig(t *testing.T) (*Pool, *Backend) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "`+closedServer(t)+`"}
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: fleet}
users:
- name: u
  user: {token: tok}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := fleet.KubeCluster{ID: handleCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: handleNS}
	pool := NewPool(PoolOptions{
		Records:     func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Environ:     func() []string { return []string{"PATH=/usr/bin:/bin"} },
		Owner:       func() (string, error) { return handleOwner, nil },
		BackoffBase: time.Millisecond, BackoffCap: 10 * time.Millisecond,
	})
	t.Cleanup(pool.Close)
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Fleet: "alpha", Instance: "web",
		StatefulSet: handleSTS(), StatefulSetUID: "uid-sts", Gen: 1, SpecHash: "sha256:h"}); err != nil {
		t.Fatal(err)
	}
	return pool, newHandle((&handleRig{}).inst(), pool)
}

// mustBeUnreachable fails unless err's message leads with
// kubernetes:unreachable (as the shim prints it, and a gRPC status carries
// it) and still says why.
func mustBeUnreachable(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error from a cluster that does not answer", what)
	}
	if code, _, ok := kubeerr.Parse(err.Error()); !ok || !code.Is(kubeerr.Unreachable) {
		t.Errorf("%s: %q does not lead with %s", what, err, kubeerr.Unreachable)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("%s: %q lost the dial's own error", what, err)
	}
}

// The exec surface names a cluster that does not answer
// kubernetes:unreachable, not client-go's bare dial error: the pod's
// verification (fleetd checks it before a terminal attach gets a
// credential), a command without a terminal (no agent connects, so it goes
// on to the paced fallback, whose error is the one the user sees) and a
// terminal (refused at once).
func TestExecSurfaceNamesAnUnreachableCluster(t *testing.T) {
	pool, b := newUnreachableRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tgt, err := b.PodTarget()
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Verify(ctx, tgt)
	mustBeUnreachable(t, "Verify", err)
	if !strings.Contains(err.Error(), "instance StatefulSet "+handleNS+"/"+handleSTS()) {
		t.Errorf("Verify: %q does not say what it read", err)
	}

	s, err := b.OpenPodExec(ctx, PodCommand{Argv: []string{"true"}})
	if err != nil {
		t.Fatalf("a command without a terminal goes on to the fallback when no agent connects: %v", err)
	}
	code, err := s.Wait()
	if code != -1 {
		t.Errorf("the fallback's exit %d, want -1", code)
	}
	mustBeUnreachable(t, "the fallback", err)

	_, err = b.OpenPodExec(ctx, PodCommand{Argv: []string{"sh"}, TTY: true, Rows: 24, Cols: 80})
	mustBeUnreachable(t, "a terminal", err)
}
