//go:build !nok8s && linux

package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec/fakekubelet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The port-forward pool's tests dial a fake kubelet (internal/kubeexec/
// fakekubelet) through client-go's real port-forward dialers; the pods are
// the harness's (verified against a fake clientset, agents run locally).

// pfRig is a harness whose pool dials kubelet.
type pfRig struct {
	*harness
	kubelet *fakekubelet.Kubelet
}

func newPFRig(t *testing.T, opts PoolOptions) *pfRig {
	t.Helper()
	k := fakekubelet.New()
	t.Cleanup(k.Close)
	opts.PortForward = func(ctx context.Context, _ *Cluster, ns, pod string) (*kubeexec.PortForward, error) {
		return kubeexec.DialPortForward(ctx, k.Config(), ns, pod)
	}
	return &pfRig{harness: newHarness(t, opts), kubelet: k}
}

// serveLocal starts a local server and routes the pods' port to it.
func (r *pfRig) serveLocal(port int, serve func(net.Conn)) {
	r.t.Helper()
	addr, stop, err := fakekubelet.Listen(serve)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(stop)
	r.kubelet.Route(port, addr)
}

func echo(c net.Conn) { _, _ = io.Copy(c, c) }

// counter answers how many bytes it read, after delay, once its client
// half-closes.
func counter(delay time.Duration) func(net.Conn) {
	return func(c net.Conn) {
		n, _ := io.Copy(io.Discard, c)
		time.Sleep(delay)
		fmt.Fprintf(c, "%d\n", n)
	}
}

// halfClose writes payload, half-closes and reads the answer to the end.
func halfClose(conn io.ReadWriteCloser, payload []byte) (string, error) {
	go func() {
		_, _ = conn.Write(payload)
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(conn)
	return string(got), err
}

func dialPort(t *testing.T, p *Pool, target PodTarget, port int) io.ReadWriteCloser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := p.DialPort(ctx, target, port)
	if err != nil {
		t.Fatalf("DialPort %d: %v", port, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPoolPortForwardSharesOneConnectionPerPod(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(80, counter(0))
	r.serveLocal(81, echo)
	a, b := r.addPod("a"), r.addPod("b")

	payload := bytes.Repeat([]byte("z"), 2<<20)
	got, err := halfClose(dialPort(t, r.pool, a, 80), payload)
	if err != nil || strings.TrimSpace(got) != strconv.Itoa(len(payload)) {
		t.Fatalf("upload + half-close: %q, %v", got, err)
	}
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("hello")); err != nil || got != "hello" {
		t.Fatalf("echo: %q, %v", got, err)
	}
	if got, err := halfClose(dialPort(t, r.pool, b, 81), []byte("other pod")); err != nil || got != "other pod" {
		t.Fatalf("echo on the second pod: %q, %v", got, err)
	}
	if n := r.kubelet.UpgradesFor(r.ns, a.Pod()); n != 1 {
		t.Fatalf("pod a's connections should share one upgrade, got %d", n)
	}
	if n := r.kubelet.UpgradesFor(r.ns, b.Pod()); n != 1 {
		t.Fatalf("pod b has its own upgrade, got %d", n)
	}
	if r.pool.PortForwards() != 2 {
		t.Fatalf("one pooled connection per pod: %d", r.pool.PortForwards())
	}
	// No exec ran: forwarding needs no agent.
	if c := r.counts(a.Pod()); len(c) != 0 {
		t.Fatalf("a forward started execs in the pod: %v", c)
	}
}

func TestPoolPortForwardRefusedPortLeavesTheOthers(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	keep := dialPort(t, r.pool, a, 81)

	refused := dialPort(t, r.pool, a, 9)
	_, err := io.ReadAll(refused)
	var perr *kubeexec.PortError
	if !errors.As(err, &perr) || perr.Port != 9 {
		t.Fatalf("a refused port should read as a *PortError: %v", err)
	}
	if got, err := halfClose(keep, []byte("open before")); err != nil || got != "open before" {
		t.Fatalf("a connection open across the refusal: %q, %v", got, err)
	}
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("after")); err != nil || got != "after" {
		t.Fatalf("a new connection after the refusal: %q, %v", got, err)
	}
	if r.kubelet.Upgrades() != 1 {
		t.Fatalf("the refusal must not cost the pod its connection: %d upgrades", r.kubelet.Upgrades())
	}
}

func TestPoolPortForwardCarriesFiftyConnections(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := r.pool.DialPort(context.Background(), a, 81)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte{byte('a' + i%26)}, 128<<10)
			if got, err := halfClose(conn, payload); err != nil || got != string(payload) {
				errs <- fmt.Errorf("connection %d: %d bytes back, %v", i, len(got), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if r.kubelet.Upgrades() != 1 || r.kubelet.Streams() != n {
		t.Fatalf("%d connections on one upgrade: upgrades=%d streams=%d", n, r.kubelet.Upgrades(), r.kubelet.Streams())
	}
}

func TestPoolPortForwardRedialsAfterADrop(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("one")); err != nil || got != "one" {
		t.Fatalf("%q, %v", got, err)
	}
	r.kubelet.Drop()
	waitFor(t, "the dropped connection to be let go", func() bool { return r.pool.PortForwards() == 0 })
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("two")); err != nil || got != "two" {
		t.Fatalf("after the drop: %q, %v", got, err)
	}
	if r.kubelet.Upgrades() != 2 {
		t.Fatalf("the next dial should open a new connection: %d upgrades", r.kubelet.Upgrades())
	}
}

func TestPoolPortForwardClosesAnIdleConnection(t *testing.T) {
	r := newPFRig(t, PoolOptions{PortForwardIdle: 100 * time.Millisecond})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	conn := dialPort(t, r.pool, a, 81)
	time.Sleep(300 * time.Millisecond)
	if r.pool.PortForwards() != 1 {
		t.Fatal("a connection carrying a TCP connection is not idle")
	}
	_ = conn.Close()
	waitFor(t, "the idle connection to close", func() bool { return r.pool.PortForwards() == 0 && r.kubelet.Open() == 0 })
}

func TestPoolPortForwardRefusesATamperedPod(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	pod, err := r.cs.CoreV1().Pods(r.ns).Get(context.Background(), a.Pod(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
	if _, err := r.cs.CoreV1().Pods(r.ns).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = r.pool.DialPort(context.Background(), a, 81)
	var tampered *TamperedError
	if !errors.As(err, &tampered) {
		t.Fatalf("a substituted pod must get no connection: %v", err)
	}
	if r.kubelet.Requests() != 0 {
		t.Fatalf("nothing should reach the cluster's port-forward: %d requests", r.kubelet.Requests())
	}
}

func TestPoolPortForwardFollowsAReplacedPod(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("old")); err != nil || got != "old" {
		t.Fatalf("%q, %v", got, err)
	}
	ctx := context.Background()
	pod, err := r.cs.CoreV1().Pods(r.ns).Get(ctx, a.Pod(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cs.CoreV1().Pods(r.ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	pod.UID, pod.ResourceVersion = types.UID("uid-a-replaced"), ""
	if _, err := r.cs.CoreV1().Pods(r.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("new")); err != nil || got != "new" {
		t.Fatalf("on the replacement pod: %q, %v", got, err)
	}
	if r.kubelet.Upgrades() != 2 || r.pool.PortForwards() != 1 {
		t.Fatalf("the replacement gets a connection of its own and the old one goes: upgrades=%d pooled=%d", r.kubelet.Upgrades(), r.pool.PortForwards())
	}
}

func TestPoolPortForwardRemembersAForbiddingCluster(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.kubelet.Forbid(true)
	a := r.addPod("a")
	for range 3 {
		_, err := r.pool.DialPort(context.Background(), a, 81)
		if !errors.Is(err, kubeexec.ErrPortForwardForbidden) {
			t.Fatalf("a forbidding cluster: %v", err)
		}
	}
	if n := r.kubelet.Requests(); n != 1 {
		t.Fatalf("the refusal should be remembered, not asked again: %d requests", n)
	}
	// Past the recheck it is asked again.
	later := time.Now().Add(portForwardForbiddenRecheck + time.Second)
	r.now.Store(&later)
	r.kubelet.Forbid(false)
	r.serveLocal(81, echo)
	if got, err := halfClose(dialPort(t, r.pool, a, 81), []byte("allowed")); err != nil || got != "allowed" {
		t.Fatalf("once allowed: %q, %v", got, err)
	}
}

func TestPoolClosePortForwards(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	conn := dialPort(t, r.pool, a, 81)
	r.pool.ClosePortForwards()
	waitFor(t, "the cluster to see the connection closed", func() bool { return r.kubelet.Open() == 0 })
	if _, err := conn.Write([]byte("x")); err == nil {
		if _, err := io.ReadAll(conn); err == nil {
			t.Fatal("a TCP connection outlived the closed pool")
		}
	}
	if _, err := r.pool.DialPort(context.Background(), a, 81); err == nil {
		t.Fatal("a closed pool should not dial")
	}
}

func TestPoolForgetClosesThePodsConnection(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.serveLocal(81, echo)
	a := r.addPod("a")
	if err := r.pool.WarmPorts(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if r.pool.PortForwards() != 1 {
		t.Fatal("WarmPorts should open the pod's connection")
	}
	r.pool.Forget(a)
	waitFor(t, "the forgotten pod's connection to close", func() bool { return r.pool.PortForwards() == 0 && r.kubelet.Open() == 0 })
}

// handleOnRig is a Kubernetes handle on an instance of the rig's pool: its
// pod, its instance record and its state.json record.
func handleOnRig(t *testing.T, r *pfRig) (*Backend, PodTarget) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	const key = "abcd2345"
	sts := names.Instance("fleet", "web", key)
	target := r.addPodSTS("web", sts)
	rec := InstanceRecord{
		Cluster: testCluster, Namespace: r.ns, Key: key, Fleet: "fleet", Instance: "web",
		StatefulSet: sts, StatefulSetUID: string(target.Expect.StatefulSetUID), Gen: 1,
		SpecHash: target.Expect.SpecHash, ImageDigests: target.Expect.ImageDigests,
	}
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	inst := &fleet.Instance{
		Name: "web", Backend: fleet.BackendKubernetes, KubeCluster: testCluster, KubeKey: key,
		WorkspaceDir: filepath.Join(state.WorkspacesDir(), "fleet", "web", "fleet"),
	}
	return newHandle(inst, r.pool), rec.target()
}

// Where the cluster forbids pods/portforward, a forward goes through the pod
// agent's forward stream instead.
func TestHandleDialPortFallsBackToTheAgent(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.kubelet.Forbid(true)
	b, _ := handleOnRig(t, r)
	// The fake pod's agent runs on this host: its loopback is the pod's.
	addr, stop, err := fakekubelet.Listen(echo)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	var dialer backend.PortDialer = b
	conn, err := dialer.DialPort(context.Background(), "", port)
	if err != nil {
		t.Fatalf("a forbidding cluster should fall back to the agent: %v", err)
	}
	defer conn.Close()
	if got, err := halfClose(conn, []byte("via the agent")); err != nil || got != "via the agent" {
		t.Fatalf("%q, %v", got, err)
	}
	if err := b.WarmPorts(context.Background(), ""); err != nil {
		t.Fatalf("warming a forbidding cluster's pod warms its agent: %v", err)
	}
	if r.kubelet.Upgrades() != 0 {
		t.Fatalf("nothing should have been upgraded: %d", r.kubelet.Upgrades())
	}
}

// The cluster's runtime drops a port-forward's half-close and ends the
// connection soon after (the fake kubelet at 200ms here, containerd at about
// a second): the answer of a peer that waits for EOF is lost on the pool's
// path and kept on the half-close path.
func TestHandleHalfCloseKeepsALateAnswer(t *testing.T) {
	r := newPFRig(t, PoolOptions{})
	r.kubelet.EarlyClose(200 * time.Millisecond)
	b, _ := handleOnRig(t, r)
	addr, stop, err := fakekubelet.Listen(counter(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)
	r.kubelet.Route(port, addr)

	conn, err := b.DialPort(context.Background(), "", port)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := halfClose(conn, []byte("12345"))
	_ = conn.Close()
	if strings.TrimSpace(got) == "5" {
		t.Fatalf("the runtime's early close should lose the late answer on the pool's path (the fake does not close early?)")
	}

	conn, err = b.DialPortHalfClose(context.Background(), "", port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got, err := halfClose(conn, []byte("12345")); err != nil || strings.TrimSpace(got) != "5" {
		t.Fatalf("the half-close path should keep the late answer: %q, %v", got, err)
	}
}
