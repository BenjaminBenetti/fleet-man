//go:build !nok8s

package server

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

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
// per run: the same run's later end events (a park, then the pod
// terminating) read nothing again.
func TestKubeLogsOnRunEndedAppendsAndDedups(t *testing.T) {
	reader := &fakeLogReader{byPrevious: map[bool]string{true: "crashed run output\n", false: "parked run output\n"}}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	end := func(count int32, why kubernetes.RunEndReason) kubernetes.RunEnd {
		return kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: count, Why: why}
	}
	// Run 2 crashed (restart 2 -> 3), then run 3 crash-looped and was parked,
	// and the park scaled its pod down (terminating): run 3's two ends.
	l.onRunEnded(context.Background(), end(2, kubernetes.RunRestarted))
	l.onRunEnded(context.Background(), end(2, kubernetes.RunRestarted)) // the same event again.
	l.onRunEnded(context.Background(), end(3, kubernetes.RunParked))
	l.onRunEnded(context.Background(), end(3, kubernetes.RunTerminating))

	data, err := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web"))
	if err != nil {
		t.Fatalf("container.log: %v", err)
	}
	got := string(data)
	if strings.Count(got, "crashed run output") != 1 || strings.Count(got, "parked run output") != 1 {
		t.Fatalf("each run must be saved exactly once:\n%s", got)
	}
	if reader.calls != 2 {
		t.Fatalf("ReadContainerLog called %d times, want 2 (one per run)", reader.calls)
	}
	saved := kubernetes.SavedRuns("alpha", "web")
	if !saved[kubernetes.RunKey("podA", 2)] || !saved[kubernetes.RunKey("podA", 3)] || len(saved) != 2 {
		t.Fatalf("SavedRuns = %v, want podA/2 and podA/3", saved)
	}
}

// The file records what is saved, so a fleetd that restarted (its memory of
// saved runs gone) does not save a run again.
func TestKubeLogsDedupSurvivesARestart(t *testing.T) {
	reader := &fakeLogReader{byPrevious: map[bool]string{false: "the run\n"}}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	e := kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 0, Why: kubernetes.RunTerminating}
	l.onRunEnded(context.Background(), e)
	restarted := newKubeLogs()
	restarted.pool = func() kubeContainerLogReader { return reader }
	restarted.onRunEnded(context.Background(), e)
	restarted.readBackRolling(context.Background(), "alpha", "web", &fakeRollingReader{runs: []podinit.RunOutput{
		{RunMark: podinit.RunMark{PodUID: "podA"}, PodSeq: 0, Data: []byte("the run\n")},
		{RunMark: podinit.RunMark{PodUID: "podB"}, PodSeq: 0, Data: []byte("current\n")},
	}})
	data, _ := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web"))
	if got := string(data); strings.Count(got, "the run") != 1 || reader.calls != 1 {
		t.Fatalf("a restarted fleetd saved a run again (%d reads):\n%s", reader.calls, got)
	}
}

// readBackRolling saves only the ended runs not saved yet (one lost with its
// node), skipping those saved from the API and the current run, which is on
// the live log.
func TestKubeLogsReadBackRollingSavesOnlyTheLostRuns(t *testing.T) {
	reader := &fakeLogReader{byPrevious: map[bool]string{true: "api output\n"}}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	// onRunEnded saved podA's run 0 (its restart) from the API.
	l.onRunEnded(context.Background(), kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 0, Why: kubernetes.RunRestarted})

	rr := &fakeRollingReader{runs: []podinit.RunOutput{
		{RunMark: podinit.RunMark{PodUID: "podA"}, PodSeq: 0, Data: []byte("api output\n")},       // saved: skip.
		{RunMark: podinit.RunMark{PodUID: "podA"}, PodSeq: 1, Data: []byte("lost node output\n")}, // lost with its node: recover.
		{RunMark: podinit.RunMark{PodUID: "podB"}, PodSeq: 0, Data: []byte("current output\n")},   // running now: skip.
	}}
	l.readBackRolling(context.Background(), "alpha", "web", rr)
	l.readBackRolling(context.Background(), "alpha", "web", rr) // the next start: nothing new.

	data, _ := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web"))
	got := string(data)
	if strings.Count(got, "api output") != 1 {
		t.Fatalf("podA's run 0 duplicated by the read-back:\n%s", got)
	}
	if strings.Count(got, "lost node output") != 1 {
		t.Fatalf("the node-lost run was not recovered exactly once:\n%s", got)
	}
	if strings.Contains(got, "current output") {
		t.Fatalf("the current run was saved while it runs (it is on the live log):\n%s", got)
	}
	// The recovered run is named as its API save would be: a later end event
	// for it reads nothing.
	l.onRunEnded(context.Background(), kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "podA", RestartCount: 1, Why: kubernetes.RunTerminating})
	if reader.calls != 1 {
		t.Fatalf("a recovered run was read again from the API (%d reads)", reader.calls)
	}
}

type fakeRollingReader struct {
	runs []podinit.RunOutput
	err  error
}

func (f *fakeRollingReader) ReadRollingLog(context.Context) ([]podinit.RunOutput, error) {
	return f.runs, f.err
}

// A reader error on run-end saves nothing, and leaves the run to a later end
// event (or the rolling read-back).
func TestKubeLogsOnRunEndedToleratesAReadError(t *testing.T) {
	reader := &fakeLogReader{err: errors.New("pod gone")}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	e := kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: "p", RestartCount: 1, Why: kubernetes.RunParked}
	l.onRunEnded(context.Background(), e)
	if _, err := os.Stat(fleetpaths.ContainerLogPath("alpha", "web")); !os.IsNotExist(err) {
		t.Fatal("a failed read should leave no container.log")
	}
	reader.err, reader.byPrevious = nil, map[bool]string{false: "second try\n"}
	e.Why = kubernetes.RunTerminating
	l.onRunEnded(context.Background(), e)
	if data, _ := os.ReadFile(fleetpaths.ContainerLogPath("alpha", "web")); !strings.Contains(string(data), "second try") {
		t.Fatalf("the run whose first read failed was not saved on its next end:\n%s", data)
	}
}

// A Fleet start of a Kubernetes instance reads its rolling copy back (runs
// that ended unsaved, e.g. while fleetd was down); another backend's start
// does nothing.
func TestKubePodsStartedReadsTheRollingCopyBack(t *testing.T) {
	newKubeLogsFixture(t, &fakeLogReader{})
	if err := state.Update(func(st *state.State) error {
		return st.Fleets["alpha"].AddInstance(&fleet.Instance{Name: "dc", Backend: fleet.BackendDevcontainer})
	}); err != nil {
		t.Fatal(err)
	}
	k := newKubePods(newService())
	got := make(chan string, 2)
	k.readBack = func(_ context.Context, fleetName string, inst *fleet.Instance) { got <- fleetName + "/" + inst.Name }
	k.started("alpha", "dc")
	k.started("alpha", "web")
	select {
	case name := <-got:
		if name != "alpha/web" {
			t.Fatalf("read back %s, want only alpha/web", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Kubernetes start did not read the rolling copy back")
	}
	select {
	case name := <-got:
		t.Fatalf("a second read-back: %s", name)
	case <-time.After(100 * time.Millisecond):
	}
}

// funcLogReader runs during before answering (a teardown racing the read).
type funcLogReader struct {
	out    string
	during func()
}

func (f *funcLogReader) ReadContainerLog(_ context.Context, _, _ string, _ bool, w io.Writer) error {
	if f.during != nil {
		f.during()
	}
	_, _ = w.Write([]byte(f.out))
	return nil
}

// The teardown removes container.log: the pod's own end events during it
// (the teardown deleting the pod) save nothing, and a save that raced the
// teardown removes its file again, so no log outlives its instance.
func TestKubeLogsNeverOutliveTheInstance(t *testing.T) {
	reader := &funcLogReader{out: "the last run\n"}
	l := newKubeLogsFixture(t, reader)
	sts := stsOf(t)
	setStatus := func(s fleet.InstanceStatus) {
		t.Helper()
		if err := state.Update(func(st *state.State) error {
			inst, _ := st.Fleets["alpha"].GetInstance("web")
			inst.Status = s
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	end := func(uid string) kubernetes.RunEnd {
		return kubernetes.RunEnd{Cluster: "mk-a", StatefulSet: sts, Pod: names.Pod(sts), PodUID: types.UID(uid), Why: kubernetes.RunTerminating}
	}
	path := fleetpaths.ContainerLogPath("alpha", "web")

	setStatus(fleet.StatusDeleting)
	l.onRunEnded(context.Background(), end("podA"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a run ending in the instance's teardown was saved")
	}

	setStatus(fleet.StatusRunning)
	reader.during = func() { setStatus(fleet.StatusDeleting) }
	l.onRunEnded(context.Background(), end("podB"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a save that raced the teardown left its container.log behind")
	}
}
