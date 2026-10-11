//go:build !nok8s && linux

package kubernetes

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The tests below run the pod's audio activity and the audio streams against
// the harness's local pods.

// harnessKey is the instance key of the pods handleOn binds a handle to.
const harnessKey = handleKey

// addInstancePod is addPod for instance name of fleet "fleet", its
// StatefulSet named as fleet names an instance's.
func (h *harness) addInstancePod(name string) PodTarget {
	h.t.Helper()
	return h.addPodAs(name, names.Instance("fleet", name, harnessKey))
}

// handleOn binds a handle on the harness's pool to the instance of target
// (addInstancePod), with fleetd's record of the pod saved, so the handle
// reaches the pod as fleetd's own would.
func (h *harness) handleOn(t *testing.T, target PodTarget, nonRoot bool) *Backend {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	pod, err := h.cs.CoreV1().Pods(h.ns).Get(context.Background(), target.Pod(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rec := InstanceRecord{Cluster: testCluster, Namespace: h.ns, Key: harnessKey, Fleet: "fleet", Instance: target.Instance,
		StatefulSet: target.StatefulSet, StatefulSetUID: string(target.Expect.StatefulSetUID), Gen: 1,
		SpecHash: testHash(&pod.Spec), ImageDigests: []string{testDigest}, NonRoot: nonRoot}
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	inst := &fleet.Instance{Name: target.Instance, Backend: fleet.BackendKubernetes, KubeCluster: testCluster, KubeKey: harnessKey,
		WorkspaceDir: filepath.Join(state.WorkspacesDir(), "fleet", target.Instance, "fleet"),
		ContainerID:  ContainerID(testCluster, h.ns, target.StatefulSet, 1)}
	return newHandle(inst, h.pool)
}

// The pod's bulk session is paced only while audio flows on it (mic demand,
// or audible output in the last 2s), and then only once a PCM frame was
// late; late frames while nothing flows never pace it. The flow is the pod's,
// shared by its sessions, and none exists for a pod the pool never met.
func TestPodAudioPacesTheBulkSessionOnlyWhileAudioFlows(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addInstancePod("one")
	b := h.handleOn(t, target, false)
	ctx := context.Background()
	if f := b.AudioFlow(b.inst.ContainerID); f != nil {
		t.Fatal("an audio flow for a pod with no session yet")
	}
	s, err := h.pool.EnsureAgent(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	flow := b.AudioFlow(b.inst.ContainerID)
	if flow == nil {
		t.Fatal("no audio flow for a pod with a session")
	}
	var paced []float64
	s.Pacer().OnPaced(func(c podwire.PaceChange) { paced = append(paced, c.Rate) })

	// Bulk traffic, for the pacer to measure.
	bulk, err := s.Bulk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 3<<16)
	if err := bulk.WriteFile(ctx, podwire.FileWriteHeader{Path: h.rootPath(target.Pod(), "big"), Mode: 0o600, Size: int64(len(big)), SHA256: hexSHA(big)}, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}

	flow.Late() // nothing flows: an open, silent stream never paces
	if s.Pacer().Rate() != 0 || len(paced) != 0 {
		t.Fatalf("paced with no audio flowing: rate %v", s.Pacer().Rate())
	}
	flow.MicDemand(true)
	if !s.Activity().Flowing() {
		t.Fatal("mic demand is not audio flowing")
	}
	flow.Late()
	if s.Pacer().Rate() < podwire.PaceFloor {
		t.Fatalf("a late frame while recording did not pace the bulk session: rate %v", s.Pacer().Rate())
	}
	flow.MicDemand(false)
	if s.Pacer().Rate() != 0 {
		t.Fatal("still paced after the recording ended")
	}
	time.Sleep(260 * time.Millisecond) // past the pacer's holdoff
	flow.OutputAudible()
	flow.Late()
	if s.Pacer().Rate() == 0 {
		t.Fatal("a late frame while output is audible did not pace the bulk session")
	}
	if len(paced) < 2 || paced[0] <= 0 {
		t.Fatalf("pacing transitions %v", paced)
	}
}

// Audio flowing when a session drops makes its reconnect interactive: the
// same signal that paces the bulk session.
func TestAudioFlowingAtTheDropReconnectsFirst(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addInstancePod("one")
	b := h.handleOn(t, target, false)
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	b.AudioFlow(b.inst.ContainerID).MicDemand(true)
	_ = s.Close()
	waitDone(t, s.Done(), "the recording session")
	waitFor(t, "the drop to count as interactive", func() bool { return h.pool.droppedInteractive(target) })
}

// An audio stream fails closed: it is not held for a resume, so a session
// that drops ends it (a microphone must not stay open on a dead path),
// while a shell is held for its reconnect.
func TestAudioStreamsAreNotHeldAcrossADrop(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addInstancePod("one")
	b := h.handleOn(t, target, false)
	ctx := context.Background()
	audio, err := b.OpenExec(ctx, backend.ExecRequest{Class: backend.ExecClassScript, Argv: []string{"sleep", "30"}, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	shell, err := b.OpenExec(ctx, backend.ExecRequest{Class: backend.ExecClassScript, Argv: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shell.Close(); _, _ = shell.Wait() })
	if audio.(podStream).Resumable() || !shell.(podStream).Resumable() {
		t.Fatalf("resumable: audio %v, shell %v; want only the shell", audio.(podStream).Resumable(), shell.(podStream).Resumable())
	}
	s := h.pool.Session(target)
	_ = s.Close()
	ended := make(chan struct{})
	go func() { _, _ = audio.Wait(); close(ended) }()
	waitDone(t, ended, "the audio stream after its session dropped")
}

// A pod without root whose image lacks the audio stack has no audio: no mic
// sink, no output source, the reason on its banner and its row, and no root
// class for a privileged step; with the stack (or with root) it has audio.
func TestAudioCapabilityFollowsTheRecord(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addInstancePod("one")
	b := h.handleOn(t, target, true)
	if !b.SupportsMicSink() || b.AudioUnavailable() != "" || b.RootCapable() {
		t.Fatalf("a pod without root, stack unchecked: mic %v, unavailable %q, root %v", b.SupportsMicSink(), b.AudioUnavailable(), b.RootCapable())
	}
	if _, ok := b.OutputSourceCommand(""); !ok {
		t.Fatal("no output source for a pod that may have audio")
	}
	rec, err := LoadInstanceRecord(testCluster, harnessKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.AudioMissing = true
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	if b.SupportsMicSink() || b.AudioReason() != AudioNoRoot {
		t.Fatalf("a pod without root or audio stack: mic %v, reason %q", b.SupportsMicSink(), b.AudioReason())
	}
	if want := AudioUnavailableMessage(testCluster); b.AudioUnavailable() != want {
		t.Fatalf("banner %q, want %q", b.AudioUnavailable(), want)
	}
	if cmd, ok := b.MicSinkCommand(""); ok || cmd != nil {
		t.Fatal("a mic sink for a pod with no audio")
	}
	if cmd, ok := b.OutputSourceCommand(""); ok || cmd != nil {
		t.Fatal("an output source for a pod with no audio")
	}

	// With root it is the root class that installs the stack.
	rec.NonRoot = false
	if err := SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	if !b.SupportsMicSink() || !b.RootCapable() || b.AudioUnavailable() != "" {
		t.Fatal("a pod with root has no audio")
	}
}

// checkAudio records a missing stack, and says so when audio is on; a stack
// that is there, or a check that cannot tell, leaves the instance's audio on.
func TestCheckAudioRecordsAMissingStack(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addInstancePod("one")
	b := h.handleOn(t, target, true)
	s, err := h.pool.EnsureAgent(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := LoadInstanceRecord(testCluster, harnessKey)
	if err != nil {
		t.Fatal(err)
	}
	origWanted := audioWanted
	t.Cleanup(func() { audioWanted = origWanted })
	for _, tc := range []struct {
		name          string
		installed     bool
		wanted        bool
		missing, warn bool
	}{
		{"installed", true, true, false, false},
		{"missing, audio on", false, true, true, true},
		{"missing, audio off", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioWanted = func() bool { return tc.wanted }
			r := rec
			c := &creation{b: b, id: bound{cluster: testCluster, fleet: "fleet", name: "one", key: harnessKey}, rec: fleet.KubeCluster{ID: testCluster, DisplayName: "lab"}}
			check := "exit 1"
			if tc.installed {
				check = "exit 0"
			}
			c.checkAudioWith(context.Background(), s, &r, check)
			saved, _ := LoadInstanceRecord(testCluster, harnessKey)
			if r.AudioMissing != tc.missing || saved.AudioMissing != tc.missing {
				t.Fatalf("missing: record %v, saved %v, want %v", r.AudioMissing, saved.AudioMissing, tc.missing)
			}
			warned := len(c.warnings) == 1 && c.warnings[0] == AudioUnavailableMessage("lab")
			if warned != tc.warn {
				t.Fatalf("warnings %q, want the unavailable line: %v", c.warnings, tc.warn)
			}
			if err := SaveInstanceRecord(rec); err != nil { // back to unchecked
				t.Fatal(err)
			}
		})
	}
}
