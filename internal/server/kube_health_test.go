//go:build !nok8s

package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// testClock is a settable clock for the kube manager.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// credentialFixture is a fixture whose minikube connection authenticates
// with the credential cred returns.
func credentialFixture(t *testing.T, cred func() (kubeexec.Credential, error)) (*kubeFixture, *testClock) {
	t.Helper()
	f := newKubeFixture(t)
	clock := &testClock{t: time.Now()}
	f.svc.kube.now = clock.now
	connect := f.svc.kube.connect
	f.svc.kube.connect = func(rec fleet.KubeCluster, environ []string) (*kubeLink, error) {
		link, err := connect(rec, environ)
		if err != nil {
			return nil, err
		}
		link.credential = func(context.Context) (kubeexec.Credential, error) { return cred() }
		return link, nil
	}
	return f, clock
}

// A credential that cannot refresh itself is warned of 30 minutes ahead and
// turns the connection UNAUTHORIZED once it expires, with no probe needed.
func TestKubeCredentialExpiryIsPushed(t *testing.T) {
	expires := time.Now().Add(2 * time.Hour)
	f, clock := credentialFixture(t, func() (kubeexec.Credential, error) {
		return kubeexec.Credential{Auth: kubeexec.AuthClientCert, ExpiresAt: expires}, nil
	})
	clock.mu.Lock()
	clock.t = expires.Add(-2 * time.Hour)
	clock.mu.Unlock()
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)

	st := w.status()
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY || st.GetCapabilities().GetCredentialsExpireAt().AsTime().Unix() != expires.Unix() {
		t.Fatalf("fresh = %v, expires %v", st.GetState(), st.GetCapabilities().GetCredentialsExpireAt())
	}
	for _, w := range st.GetWarnings() {
		if strings.Contains(w, "credentials expire") {
			t.Fatalf("warned two hours ahead: %v", st.GetWarnings())
		}
	}
	clock.advance(time.Hour + 40*time.Minute)
	st = w.status()
	if !strings.Contains(strings.Join(st.GetWarnings(), "\n"), "credentials expire in 20m") || st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Errorf("20 minutes ahead = %v %v", st.GetState(), st.GetWarnings())
	}
	// The worker wakes for the warning and the expiry, not just its probes.
	w.mu.Lock()
	_, wait := w.nextLocked(clock.now())
	w.mu.Unlock()
	if wait > 20*time.Minute {
		t.Errorf("next wake in %v, after the expiry", wait)
	}
	clock.advance(2 * time.Hour)
	if st = w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED || st.GetMessage() != "credentials expired 1h ago" {
		t.Errorf("after expiry = %v %q", st.GetState(), st.GetMessage())
	}
}

// A plugin credential refreshes itself: its expiry passing is no warning,
// but a re-read that fails (the login behind it lapsed) is UNAUTHORIZED.
func TestKubeRefreshableCredentialRecheck(t *testing.T) {
	var mu sync.Mutex
	fail := false
	expires := time.Now().Add(15 * time.Minute)
	f, clock := credentialFixture(t, func() (kubeexec.Credential, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return kubeexec.Credential{}, &kubeexec.PluginError{Path: "/usr/bin/aws", ExitCode: 255, Stderr: "Token has expired and refresh failed"}
		}
		return kubeexec.Credential{Auth: kubeexec.AuthExecPlugin, ExpiresAt: expires, Refreshable: true}, nil
	})
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)
	clock.advance(20 * time.Minute)
	if st := w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY || len(st.GetWarnings()) != 1 {
		t.Fatalf("a refreshable credential past its expiry = %v %v", st.GetState(), st.GetWarnings())
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	w.runShallow(context.Background())
	st := w.status()
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED || !strings.Contains(st.GetMessage(), "refresh failed") {
		t.Errorf("after a failed refresh = %v %q", st.GetState(), st.GetMessage())
	}
}

// A shallow probe that finds the API server gone makes the connection
// UNREACHABLE, keeping what the last full run learned; once it answers again
// a full run is asked for.
func TestKubeShallowProbeFailureAndRecovery(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)

	var mu sync.Mutex
	down := true
	f.apis["minikube"].Discovery().(*fakediscovery.FakeDiscovery).PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return true, nil, errors.New("dial tcp 127.0.0.1:8443: connect: connection refused")
		}
		return false, nil, nil
	})
	w.runShallow(context.Background())
	st := w.status()
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE || st.GetCapabilities().GetVersion() != "v1.37.0" {
		t.Fatalf("after a failed probe = %v %q, capabilities %v", st.GetState(), st.GetMessage(), st.GetCapabilities())
	}
	w.mu.Lock()
	full, wait := w.nextLocked(f.svc.kube.now())
	w.mu.Unlock()
	if full || wait < kubeRetryInterval*8/10 {
		t.Errorf("a failing connection retries in %v (full %v), want about %v", wait, full, kubeRetryInterval)
	}

	mu.Lock()
	down = false
	mu.Unlock()
	w.runShallow(context.Background())
	w.mu.Lock()
	full, _ = w.nextLocked(f.svc.kube.now())
	w.mu.Unlock()
	if !full {
		t.Error("recovery did not ask for a full preflight")
	}
	if st := w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Errorf("recovered = %v", st.GetState())
	}
}

// A probe that does not return (client-go runs a credential plugin with no
// deadline) is reported instead of leaving the connection looking fine.
func TestKubeStuckProbeIsReported(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	done := w.watchdog("full preflight", 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := w.status()
		if st.GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE && strings.Contains(st.GetMessage(), "has not returned") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck probe = %v %q", st.GetState(), st.GetMessage())
		}
		time.Sleep(5 * time.Millisecond)
	}
	done()
	if st := w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING {
		t.Errorf("after the probe returned = %v", st.GetState())
	}
}

// An edit asks for a full run; the daemon's own bookkeeping (server,
// cluster id, verified_at) does not.
func TestKubeWorkerRecordChanges(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	rec := f.stored(id)
	w := f.svc.kube.newWorker(rec)
	w.runFull(context.Background(), nil)
	w.mu.Lock()
	wanted := w.fullWanted
	w.mu.Unlock()
	if wanted {
		t.Fatal("a full run left a full run wanted")
	}
	noted := rec
	noted.Server, noted.ClusterUID, noted.VerifiedAt = "https://elsewhere", "uid", time.Now()
	w.setRecord(noted)
	w.mu.Lock()
	wanted, linked := w.fullWanted, w.link != nil
	w.mu.Unlock()
	if wanted || !linked {
		t.Errorf("bookkeeping: full wanted %v, link kept %v", wanted, linked)
	}
	edited := noted
	edited.MemoryLimit = "16Gi"
	w.setRecord(edited)
	w.mu.Lock()
	wanted, linked = w.fullWanted, w.link != nil
	w.mu.Unlock()
	if !wanted || !linked {
		t.Errorf("an edit: full wanted %v, link kept %v", wanted, linked)
	}
	moved := edited
	moved.Namespace = "team"
	w.setRecord(moved)
	w.mu.Lock()
	linked = w.link != nil
	w.mu.Unlock()
	if linked {
		t.Error("a re-pointed connection kept its old client")
	}
}

// A failure only a full run sees (here a missing namespace) is not
// "recovered" by every passing shallow probe: no full run every minute.
func TestKubeShallowProbeDoesNotRerunFullOnContentFailures(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "missing-ns"}).GetCluster().GetId()
	w := f.svc.kube.newWorker(f.stored(id))
	w.runFull(context.Background(), nil)
	if st := w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED {
		t.Fatalf("missing namespace = %v %q", st.GetState(), st.GetMessage())
	}
	w.runShallow(context.Background())
	w.mu.Lock()
	full, _ := w.nextLocked(f.svc.kube.now())
	w.mu.Unlock()
	if full {
		t.Error("a passing shallow probe asked for a full run of a namespace that is still missing")
	}
	if st := w.status(); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED {
		t.Errorf("after the shallow probe = %v", st.GetState())
	}
}
