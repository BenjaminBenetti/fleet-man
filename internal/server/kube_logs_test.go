//go:build !nok8s

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeLogReader stands in for the pool's ReadContainerLog.
type fakeLogReader struct {
	byPrevious map[bool]string
	err        error
	calls      int
}

func (f *fakeLogReader) ReadContainerLog(_ context.Context, _, _ string, previous bool, w io.Writer) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	_, _ = w.Write([]byte(f.byPrevious[previous]))
	return nil
}

func newKubeLogsFixture(t *testing.T, reader kubeContainerLogReader) *kubeLogs {
	t.Helper()
	isolateFleetDir(t)
	key := names.InstanceKey("d12", "alpha", "web", time.Unix(1700000000, 0).UTC())
	if err := state.Update(func(st *state.State) error {
		return st.GetOrCreateFleet("alpha", "https://example.invalid/a.git").AddInstance(
			&fleet.Instance{Name: "web", Backend: fleet.BackendKubernetes, KubeCluster: "mk-a", KubeKey: key})
	}); err != nil {
		t.Fatal(err)
	}
	l := newKubeLogs()
	l.pool = func() kubeContainerLogReader { return reader }
	t.Cleanup(func() { kubernetes.RemoveContainerLog("alpha", "web") })
	return l
}

func stsOf(t *testing.T) string {
	t.Helper()
	st, _ := state.Load()
	inst, _ := st.Fleets["alpha"].GetInstance("web")
	return names.Instance("alpha", "web", inst.KubeKey)
}

// onRunEnded appends the ending run's output (the previous container on a
// restart, the current one on a park) to the instance's container.log, once
// per run, and records the pod as saved.
func TestKubeLogsOnRunEndedAppendsAndDedups(t *testing.T) {
	reader := &fakeLogReader{byPrevious: map[bool]string{true: "crashed run output\n", false: "parked run output\n"}}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	restart := kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 2, Why: kubernetes.RunRestarted}
	l.onRunEnded(context.Background(), restart)
	l.onRunEnded(context.Background(), restart) // same key: no second read.
	park := kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 2, Why: kubernetes.RunParked}
	l.onRunEnded(context.Background(), park)

	data, err := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web"))
	if err != nil {
		t.Fatalf("container.log: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "crashed run output") || !strings.Contains(got, "parked run output") {
		t.Fatalf("container.log missing a run's output:\n%s", got)
	}
	if strings.Count(got, "crashed run output") != 1 {
		t.Fatalf("the restart run was saved more than once:\n%s", got)
	}
	if reader.calls != 2 {
		t.Fatalf("ReadContainerLog called %d times, want 2 (the duplicate restart skipped)", reader.calls)
	}
	if !l.savedPods["podA"] {
		t.Fatal("the saved pod was not recorded for the rolling read-back")
	}
}

// readBackRolling saves only the runs onRunEnded did not (a different pod,
// lost to node failure), skipping those already saved from the API.
func TestKubeLogsReadBackRollingSkipsSavedPods(t *testing.T) {
	reader := &fakeLogReader{byPrevious: map[bool]string{true: "api output\n"}}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	// onRunEnded saved podA's output from the API.
	l.onRunEnded(context.Background(), kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 1, Why: kubernetes.RunRestarted})

	rr := &fakeRollingReader{runs: []podinit.RunOutput{
		{RunMark: podinit.RunMark{PodUID: "podA"}, Data: []byte("api output\n")},          // already saved: skip.
		{RunMark: podinit.RunMark{PodUID: "podLost"}, Data: []byte("lost node output\n")}, // never saved: recover.
	}}
	l.readBackRolling(context.Background(), "alpha", "web", rr)

	data, _ := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web"))
	got := string(data)
	if strings.Count(got, "api output") != 1 {
		t.Fatalf("podA's output duplicated by the read-back:\n%s", got)
	}
	if !strings.Contains(got, "lost node output") {
		t.Fatalf("the node-lost run was not recovered:\n%s", got)
	}
}

type fakeRollingReader struct {
	runs []podinit.RunOutput
	err  error
}

func (f *fakeRollingReader) ReadRollingLog(context.Context) ([]podinit.RunOutput, error) {
	return f.runs, f.err
}

// A reader error on run-end is silent and saves nothing.
func TestKubeLogsOnRunEndedToleratesAReadError(t *testing.T) {
	l := newKubeLogsFixture(t, &fakeLogReader{err: errors.New("pod gone")})
	sts := stsOf(t)
	l.onRunEnded(context.Background(), kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "p", RestartCount: 1, Why: kubernetes.RunTerminating})
	if _, err := os.Stat(fleetpaths.ContainerLogPath("alpha", "web")); !os.IsNotExist(err) {
		t.Fatal("a failed read should leave no container.log")
	}
	if l.savedPods["p"] {
		t.Fatal("a pod whose read failed must not be marked saved")
	}
}

// AppendContainerLog keeps the file under the cap, dropping whole leading
// lines.
func TestAppendContainerLogCaps(t *testing.T) {
	isolateFleetDir(t)
	t.Cleanup(func() { kubernetes.RemoveContainerLog("f", "i") })
	line := strings.Repeat("x", 1000) + "\n"
	for i := 0; i < kubernetes.ContainerLogMax/len(line)+50; i++ {
		if err := kubernetes.AppendContainerLog("f", "i", []byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(fleetpaths.ContainerLogPath("f", "i"))
	if len(data) > kubernetes.ContainerLogMax {
		t.Fatalf("container.log is %d bytes, over the %d cap", len(data), kubernetes.ContainerLogMax)
	}
	if bytes.HasPrefix(data, []byte("x")) && !bytes.HasPrefix(data, []byte(line)) {
		t.Fatal("the front was cut mid-line")
	}
}
