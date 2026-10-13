//go:build !nok8s

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// fakeAgent is the pod agent's end of one direct-mode exec transport (`pod-agent
// exec --stdio`): it reads the preamble, answers with first, runs then (frames
// to the client; nil for none) while it keeps reading what the client sends
// until the client goes.
func fakeAgent(t *testing.T, first podwire.FrameType, reply any, then func(w *podwire.FrameWriter, conn net.Conn)) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	w := podwire.NewFrameWriter(a)
	go func() {
		if _, _, err := podwire.ReadPreamble(a); err != nil {
			return
		}
		if err := w.WriteJSON(first, reply); err != nil {
			return
		}
		if then != nil {
			go then(w, a)
		}
		r := podwire.NewFrameReader(a)
		for {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	}()
	return b
}

// openDropped opens a resumable direct exec whose first transport then drops
// (its agent end closes, as an API server cut does).
func openDropped(t *testing.T) *podwire.Exec {
	t.Helper()
	id := strings.Repeat("cd", 16)
	conn := fakeAgent(t, podwire.FrameOpen, podwire.OpenReply{ResumeID: id, PID: 9}, func(_ *podwire.FrameWriter, c net.Conn) {
		time.Sleep(20 * time.Millisecond)
		_ = c.Close()
	})
	h := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh"}}, Resumable: true}
	e, err := podwire.OpenExec(context.Background(), conn, h, podwire.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func waitExec(t *testing.T, e *podwire.Exec) (int, error) {
	t.Helper()
	type res struct {
		code int
		err  error
	}
	ch := make(chan res, 1)
	go func() { c, err := e.Wait(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		return r.code, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("the attach never ended: the drop was not resumed")
	}
	return 0, nil
}

// A direct attach whose transport drops after it started is re-attached over
// a new one, which carries it to its end.
func TestKubeExecResumeReattachesAfterADrop(t *testing.T) {
	e := openDropped(t)
	var dials int
	var mu sync.Mutex
	go kubeExecResume(context.Background(), e, func(context.Context, int) (io.ReadWriteCloser, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return fakeAgent(t, podwire.FrameResume, podwire.ResumeReply{PID: 9}, func(w *podwire.FrameWriter, _ net.Conn) {
			_ = w.Write(podwire.FrameData, []byte("back"))
			_ = w.WriteJSON(podwire.FrameExit, podwire.ExitStatus{Code: 3})
		}), nil
	})
	var out bytes.Buffer
	drained := copyOutput(e.Stdout(), e.Stderr(), &out, io.Discard)
	code, err := waitExec(t, e)
	drained()
	if code != 3 || err != nil || out.String() != "back" {
		t.Fatalf("after the drop: code %d, err %v, stdout %q; want 3, nil, back", code, err, out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Fatalf("%d re-dials, want 1", dials)
	}
}

// A re-attach that finds the held command gone (a new pod) ends the attach
// with exit 75, the wrappers' rejoin signal.
func TestKubeExecResumeOnAReplacedPodExits75(t *testing.T) {
	e := openDropped(t)
	go kubeExecResume(context.Background(), e, func(context.Context, int) (io.ReadWriteCloser, error) {
		return fakeAgent(t, podwire.FrameOpen, podwire.OpenReply{Gone: true}, func(*podwire.FrameWriter, net.Conn) {}), nil
	})
	code, err := attachExit(waitExec(t, e))
	if code != podwire.ExitHeldStreamGone || err != nil {
		t.Fatalf("a replaced pod: exit %d, %v; want %d", code, err, podwire.ExitHeldStreamGone)
	}
}

// fleetd refusing the pod on a re-dial (it no longer verifies) ends the
// attach; nothing more is sent to the pod. A re-dial that fails for now (the
// pod is still being replaced) is tried again.
func TestKubeExecResumeStopsWhenFleetdRefuses(t *testing.T) {
	e := openDropped(t)
	refused := fmt.Errorf("%w: tampered", errKubeExecRefused)
	var mu sync.Mutex
	dials := 0
	go kubeExecResume(context.Background(), e, func(context.Context, int) (io.ReadWriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return nil, errors.New("the instance pod is not running")
		}
		return nil, refused
	})
	if _, err := waitExec(t, e); !errors.Is(err, errKubeExecRefused) {
		t.Fatalf("after a refusal: %v, want the refusal", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 {
		t.Fatalf("%d re-dials, want 2 (a not-now failure is tried again)", dials)
	}
}

// podAgent is the agent end of one direct-mode exec transport in the pod
// whose uid is uid, answering as `pod-agent exec --stdio` does: a re-attach
// finds no held command (this pod replaced the one that held it), an exec
// that names another pod is refused before anything runs (the shim's
// verification was of the pod this one replaced), and any other exec starts
// and exits 0.
func podAgent(t *testing.T, uid string) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		defer a.Close()
		_, raw, err := podwire.ReadPreamble(a)
		var h podwire.ExecHeader
		if err != nil || podwire.DecodeHeader(raw, &h) != nil {
			return
		}
		w := podwire.NewFrameWriter(a)
		switch {
		case h.Resume != "":
			_ = w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{Gone: true})
			return
		case h.PodUID != "" && h.PodUID != uid:
			_ = w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{Refused: podwire.RefusedOtherPod, Error: fmt.Sprintf("this is pod %s, not %s", uid, h.PodUID)})
			return
		}
		if w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{ResumeID: strings.Repeat("ef", 16), PID: 7}) != nil ||
			w.WriteJSON(podwire.FrameExit, podwire.ExitStatus{}) != nil {
			return
		}
		r := podwire.NewFrameReader(a)
		for {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	}()
	return b
}

// shortWaits makes an attach's waits for its pod short.
func shortWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	backoff, was := redialBackoff, attachWait
	redialBackoff = []time.Duration{0, time.Millisecond, 2 * time.Millisecond}
	attachWait = wait
	t.Cleanup(func() { redialBackoff, attachWait = backoff, was })
}

// A replaced pod's pane rejoins though fleetd has not taken the replacement
// in yet (its watch of the cluster lags, as after an API server outage): the
// attach's re-dial, which fleetd still verifies as the pod that is gone,
// reaches the new pod and finds its command gone (exit 75); the wrapper's new
// attach, verified as the gone pod again, is refused by the new pod's agent,
// and asks fleetd again until it vouches for the new pod, where it starts.
func TestKubeExecRejoinOutwaitsFleetdsView(t *testing.T) {
	shortWaits(t, 10*time.Second)
	var mu sync.Mutex
	asks := 0
	vouch := func() string {
		mu.Lock()
		defer mu.Unlock()
		asks++
		if asks <= 3 {
			// The re-dial's ask and the new attach's first two.
			return "uid-old"
		}
		return "uid-new"
	}
	e := openDropped(t)
	go kubeExecResume(context.Background(), e, func(context.Context, int) (io.ReadWriteCloser, error) {
		vouch()
		return podAgent(t, "uid-new"), nil
	})
	if code, err := attachExit(waitExec(t, e)); code != podwire.ExitHeldStreamGone || err != nil {
		t.Fatalf("the replaced pod's attach: exit %d, %v; want %d", code, err, podwire.ExitHeldStreamGone)
	}

	header := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh"}}, Resumable: true}
	rejoined, err := kubeExecOpen(context.Background(), func(ctx context.Context) (*podwire.Exec, error) {
		h := header
		h.PodUID = vouch()
		return podwire.OpenExec(ctx, podAgent(t, "uid-new"), h, podwire.ExecOptions{})
	})
	if err != nil {
		t.Fatalf("the rejoin: %v; want it started once fleetd vouched for the new pod", err)
	}
	if code, err := waitExec(t, rejoined); code != 0 || err != nil {
		t.Fatalf("the rejoined command: exit %d, %v; want 0", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asks != 4 {
		t.Fatalf("fleetd was asked %d times, want 4 (the re-dial, two refused starts, the start)", asks)
	}
}

// An attach asks fleetd again while it answers not now; it stops at once when
// fleetd refuses the pod (a substituted one: nothing starts in it) or the
// start fails another way, and gives up after attachWait on a pod fleetd never
// vouches for.
func TestKubeExecOpenStops(t *testing.T) {
	refused := fmt.Errorf("%w: tampered", errKubeExecRefused)
	notNow := fleetdAnswer(status.Error(codes.FailedPrecondition, "the instance pod is not running"))
	header := podwire.ExecHeader{Request: execas.Request{Class: execas.ClassExec, Argv: []string{"sh"}}, Resumable: true}
	for _, tc := range []struct {
		name string
		wait time.Duration
		// answer is fleetd's answer to its ask n (from 1): an error, or the
		// uid it vouches for (the pod is uid-new).
		answer func(n int) (string, error)
		want   func(error) bool
		asks   int
	}{
		{"not now, then the pod", 10 * time.Second, func(n int) (string, error) {
			if n == 1 {
				return "", notNow
			}
			return "uid-new", nil
		}, func(err error) bool { return err == nil }, 2},
		{"a substituted pod", 10 * time.Second, func(n int) (string, error) {
			switch n {
			case 1:
				return "", notNow
			case 2:
				return "uid-old", nil
			}
			return "", refused
		}, func(err error) bool { return errors.Is(err, errKubeExecRefused) }, 3},
		{"fleetd cannot issue", 10 * time.Second, func(int) (string, error) {
			return "", fleetdAnswer(status.Error(codes.Unauthenticated, "the credential plugin failed"))
		}, func(err error) bool { return err != nil && !errors.Is(err, errKubeExecNotNow) }, 1},
		{"never vouched for", 50 * time.Millisecond, func(int) (string, error) { return "uid-old", nil },
			func(err error) bool {
				var r *podwire.StreamRefusedError
				return errors.As(err, &r) && r.Reason == podwire.RefusedOtherPod
			}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortWaits(t, tc.wait)
			asks := 0
			e, err := kubeExecOpen(context.Background(), func(ctx context.Context) (*podwire.Exec, error) {
				asks++
				uid, err := tc.answer(asks)
				if err != nil {
					return nil, err
				}
				h := header
				h.PodUID = uid
				return podwire.OpenExec(ctx, podAgent(t, "uid-new"), h, podwire.ExecOptions{})
			})
			if !tc.want(err) {
				t.Fatalf("open: %v", err)
			}
			if e != nil {
				if code, err := waitExec(t, e); code != 0 || err != nil {
					t.Fatalf("the command: exit %d, %v", code, err)
				}
			}
			if tc.asks > 0 && asks != tc.asks {
				t.Fatalf("%d asks, want %d", asks, tc.asks)
			}
			if tc.asks < 0 && asks < 3 {
				t.Fatalf("%d asks: gave up before attachWait", asks)
			}
		})
	}
}

// fleetd's answers to an issue, as the shim reads them: away (go to the
// kubeconfig), a refusal for good, or not now.
func TestFleetdIssueErrors(t *testing.T) {
	for _, tc := range []struct {
		code                 codes.Code
		unavailable, refused bool
	}{
		{codes.Unavailable, true, false},
		{codes.Unimplemented, true, false},
		{codes.NotFound, false, true},
		{codes.PermissionDenied, false, true},
		{codes.FailedPrecondition, false, false},
		{codes.DeadlineExceeded, false, false},
	} {
		err := status.Error(tc.code, "x")
		if fleetdUnavailable(err) != tc.unavailable || fleetdRefused(err) != tc.refused {
			t.Errorf("%v: unavailable %v refused %v, want %v %v", tc.code, fleetdUnavailable(err), fleetdRefused(err), tc.unavailable, tc.refused)
		}
	}
	// Of fleetd's other answers, a pod it cannot vouch for now is one an
	// attach waits out; each keeps fleetd's message as is.
	for _, tc := range []struct {
		code   codes.Code
		notNow bool
	}{
		{codes.FailedPrecondition, true},
		{codes.DeadlineExceeded, true},
		{codes.Unauthenticated, false},
		{codes.Internal, false},
	} {
		msg := "kubernetes:unreachable: instance pod ns/p"
		err := fleetdAnswer(status.Error(tc.code, msg))
		if errors.Is(err, errKubeExecNotNow) != tc.notNow || err.Error() != msg {
			t.Errorf("%v: %q, not now %v; want %q, %v", tc.code, err, errors.Is(err, errKubeExecNotNow), msg, tc.notNow)
		}
	}
}

// The shim's exit for each way an attach ends.
func TestAttachExit(t *testing.T) {
	for _, tc := range []struct {
		code int
		err  error
		want int
	}{
		{0, nil, 0},
		{7, nil, 7},
		{-1, podwire.ErrHeldStreamGone, podwire.ExitHeldStreamGone},
		{-1, podwire.ErrConnectionLost, 1},
		{-1, errors.New("refused"), 1},
	} {
		if got, err := attachExit(tc.code, tc.err); got != tc.want || err != nil {
			t.Errorf("attachExit(%d, %v) = %d, %v; want %d", tc.code, tc.err, got, err, tc.want)
		}
	}
}

// copyOutput's wait returns only once both streams are copied out: the shim
// exits right after, so nothing the command printed last may be left behind.
func TestCopyOutputDrainsBeforeTheWaitReturns(t *testing.T) {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	var out, errOut syncBuffer
	drained := copyOutput(outR, errR, &out, &errOut)
	go func() {
		_, _ = outW.Write([]byte("last words"))
		_ = outW.Close()
		time.Sleep(20 * time.Millisecond)
		_, _ = errW.Write([]byte("tail"))
		_ = errW.Close()
	}()
	drained()
	if out.String() != "last words" || errOut.String() != "tail" {
		t.Fatalf("drained before the copy finished: stdout %q stderr %q", out.String(), errOut.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The agent exec failing because the pod has no fleet binary (the runtime's
// 126/127 for the kube exec, or the agent's start error) is what sends an
// attach to raw mode; anything else is not.
func TestIsAgentMissing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&podwire.StartError{Code: execas.ExitNotFound}, true},
		{&kubeexec.ExitError{Code: 127}, true},
		{&kubeexec.ExitError{Code: 126}, true},
		{errors.Join(errors.New("start exec"), &kubeexec.ExitError{Code: 127}), true},
		{&kubeexec.ExitError{Code: 1}, false},
		{errors.New("connection refused"), false},
	} {
		if got := isAgentMissing(tc.err); got != tc.want {
			t.Errorf("isAgentMissing(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// downKubeconfig is a kubeconfig whose one context's API server does not
// answer (a loopback port nothing listens on).
func downKubeconfig(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://`+addr+`"}
contexts:
- name: ctx
  context: {cluster: c, user: u, namespace: team}
users:
- name: u
  user: {token: tok}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// With fleetd away, the shim reaches the cluster with the kubeconfig itself:
// a cluster that does not answer fails each of its cluster calls (the
// agent's attach, the raw exec, the live log) with kubernetes:unreachable
// leading the message, not client-go's bare dial error.
func TestKubeExecNamesAnUnreachableCluster(t *testing.T) {
	f := kubeExecFlags{
		socket: filepath.Join(t.TempDir(), "no-fleetd.sock"), cluster: "kc-1", context: "ctx",
		kubeconfig: []string{downKubeconfig(t)}, namespace: "team", pod: "fi-x-0", container: "dev", class: "exec",
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	check := func(what string, err error) {
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
	code, err := runKubeExecDirect(ctx, f, []string{"true"}, false)
	if code != -1 {
		t.Errorf("agent attach: exit %d, want -1", code)
	}
	check("agent attach", err)
	raw := f
	raw.raw = true
	_, err = runKubeExecDirect(ctx, raw, []string{"true"}, false)
	check("raw exec", err)
	check("logs", runKubeExecLogs(ctx, kubeExecLogsFlags{kubeExecFlags: f}))
}
