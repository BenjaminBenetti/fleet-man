//go:build !nok8s

package kubernetes

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/startup"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// audio.go is the instance's audio on a cluster: the virtual microphone's
// sink and the audio output's source run on the pod's agent session (the
// daemon's mic and output hubs open them as streams, backend.StreamExecer),
// fail closed (not held for a resume: a dead path must end them), and feed
// the pod's audio activity, which paces its bulk session only while audio
// actually flows (backend.AudioFlows). The audio stack is installed through
// the root class (backend.RootExecer); a pod without root (capability tier
// C) has audio only when its image carries the stack (a create adds it to
// the images fleet builds there: addAudioLayer), and otherwise says so
// (checkAudio).

// The handle's audio interfaces.
var (
	_ backend.OutputBackend = (*Backend)(nil)
	_ backend.AudioGate     = (*Backend)(nil)
	_ backend.AudioFlows    = (*Backend)(nil)
	_ backend.RootExecer    = (*Backend)(nil)
)

// AudioNoRoot is the reason an instance has no audio on a cluster that runs
// containers without root, on an image without the audio stack; the runtime
// field the Settings page counts by (KubeInstanceRuntime.audio_unavailable).
const AudioNoRoot = "no root"

// audioRecord is the instance's record, nil without one (an instance whose
// create has not applied its pod yet, or a lost ~/.fleet): such an instance is
// taken to be able to have audio, as one on a cluster with root can.
func (b *Backend) audioRecord() *InstanceRecord {
	id, err := b.identity()
	if err != nil {
		return nil
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return nil
	}
	return &rec
}

// AudioReason is why the instance can have no audio, as the runtime field
// carries it (AudioNoRoot); "" when it can.
func (b *Backend) AudioReason() string {
	if rec := b.audioRecord(); rec != nil && rec.NonRoot && rec.AudioMissing {
		return AudioNoRoot
	}
	return ""
}

// AudioUnavailable is why the instance can have no audio, as its banner
// says it (backend.AudioGate): a pod without root whose image lacks the
// audio stack.
func (b *Backend) AudioUnavailable() string {
	if b.AudioReason() == "" {
		return ""
	}
	return AudioUnavailableMessage(b.clusterName())
}

// AudioUnavailableMessage is the banner line of an instance with no audio on
// cluster name.
func AudioUnavailableMessage(cluster string) string {
	return "audio unavailable on cluster " + cluster + ": containers can't run as root. Use an image with PulseAudio and ALSA installed."
}

// clusterName is the instance's connection by the name the user gave it (its
// id when its record is gone).
func (b *Backend) clusterName() string {
	if rec, err := b.clusterRecord(); err == nil {
		return rec.GetDisplayName()
	}
	return b.inst.KubeCluster
}

// SupportsMicSink is true unless the instance can have no audio
// (AudioUnavailable). Unlike other backends' it reads a file, the instance's
// record on the daemon host (never the cluster): what its create found.
func (b *Backend) SupportsMicSink() bool { return b.AudioReason() == "" }

// MicSinkCommand is `fleet mic sink` through the kube-exec shim, as the
// session user (the script class). Fleetd itself opens the sink on the
// agent session instead (OpenExec), which fails closed.
func (b *Backend) MicSinkCommand(string) (*exec.Cmd, bool) {
	if !b.SupportsMicSink() {
		return nil, false
	}
	return b.kubeExecCmd(execas.ClassScript, "", "", shimQuiet, fleetlaunch.MicSinkArgv()), true
}

// OutputSourceCommand is `fleet output source` through the kube-exec shim,
// when the instance can have audio (backend.OutputBackend); fleetd opens the
// source on the agent session instead (OpenExec).
func (b *Backend) OutputSourceCommand(string) (*exec.Cmd, bool) {
	if !b.SupportsMicSink() {
		return nil, false
	}
	return b.kubeExecCmd(execas.ClassScript, "", "", shimQuiet, fleetlaunch.OutputSourceArgv()), true
}

// RootCapable reports whether the pod has root (backend.RootExecer): on a
// cluster that runs every container as one non-root uid it has none.
func (b *Backend) RootCapable() bool {
	rec := b.audioRecord()
	return rec == nil || !rec.NonRoot
}

// RootExecCommand is ExecCommand as root (the root class), logging one timed
// "container exec" entry when the caller runs it.
func (b *Backend) RootExecCommand(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(b.kubeExecCmd(execas.ClassRoot, "", "", 0, command), func(d time.Duration, err error) {
		flog.ContainerExec("kubernetes", workspaceDir, command, d, err)
	})
}

// AudioFlow is the pod's audio activity (backend.AudioFlows): nil until the
// pool has a slot for the pod (no session was ever made with it).
func (b *Backend) AudioFlow(containerID string) backend.AudioFlow {
	w, err := b.where(containerID)
	if err != nil {
		return nil
	}
	slot := b.p().podSlotOf(w.cluster, w.sts)
	if slot == nil {
		return nil
	}
	return podAudio{slot}
}

// podSlotOf is the pool's slot for the pod of StatefulSet sts on connection
// clusterID, nil when it has none. It loads the connection (for its
// namespace) but makes no session.
func (p *Pool) podSlotOf(clusterID, sts string) *podSlot {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slots[podKey{c.Record.ID, c.Namespace(), names.Pod(sts)}]
}

// podAudio is a pod's audio activity as the audio paths feed it: shared by
// the pod's sessions (it outlives a reconnect, and a drop while audio flows
// reconnects at interactive priority), and its late frames pace the bulk
// session of whichever session is live.
type podAudio struct{ slot *podSlot }

func (a podAudio) MicDemand(recording bool) {
	n := 0
	if recording {
		n = 1
	}
	a.slot.activity.SetMicDemand(n)
}

func (a podAudio) OutputAudible() { a.slot.activity.OutputAudible() }

func (a podAudio) Late() {
	if s := a.slot.live(); s != nil {
		s.pacer.Late()
	}
}

// audioLayerScript is the layer's file running the audio script's system
// step (startup.AudioScript's Privileged half) at build time, as root.
const audioLayerScript = "fleet-audio.sh"

// addAudioLayer gives an image fleet built for a pod without root the audio
// stack, in one layer built as root on the daemon host (the pod never gets
// root to install it): the audio script's system step, packages and /etc
// configuration, so the virtual microphone and audio output work there. A
// failure leaves the image as it was, with a warning (the instance then has
// audio only if the image already had the stack, which its pod is checked
// for: checkAudio).
func (c *creation) addAudioLayer(ctx context.Context, capture *devcli.CaptureResult) {
	backend.ReportStep(ctx, fleet.StepImageBuild, "the audio layer (the cluster runs containers without root)")
	img, err := devcli.AddLayer(ctx, devcli.LayerRequest{
		Tools: c.env.Tools, From: capture.Run.Image, Base: capture.Image, Platform: c.facts.Platform,
		Files: map[string]string{audioLayerScript: startup.AudioScript(true, true).Privileged},
		Run:   "sh " + audioLayerScript,
	})
	if err != nil {
		if ctx.Err() == nil {
			c.warn(ctx, fmt.Sprintf("adding the audio stack to the image for cluster %s (it runs containers without root): %v", c.rec.GetDisplayName(), err))
		}
		return
	}
	flog.Info("kube create: audio layer added", "fleet", c.id.fleet, "instance", c.id.name, "cluster", c.id.cluster, "base", capture.Image.ID, "image", img.ID)
	capture.Image, capture.Run.Image = img, img.ID
}

// checkAudio records whether a pod without root has the audio stack (its
// image's, or the layer fleet added), which nothing in it can install: when
// it has not, the instance has no audio (AudioUnavailable), and, with either
// audio direction on, the create says so on its banner: the finding a user of
// a prebuilt image on such a cluster needs. A check that cannot tell leaves
// the instance taken to have audio (its sink then says what is missing).
func (c *creation) checkAudio(ctx context.Context, sess *AgentSession, rec *InstanceRecord) {
	c.checkAudioWith(ctx, sess, rec, startup.AudioInstalledScript())
}

// checkAudioWith is checkAudio with the stack check's script (a test's).
func (c *creation) checkAudioWith(ctx context.Context, sess *AgentSession, rec *InstanceRecord, check string) {
	ok, err := detectAudio(ctx, sess, check)
	if err != nil {
		flog.Warn("kube create: could not check the image for the audio stack", "fleet", c.id.fleet, "instance", c.id.name, "err", err)
		return
	}
	if ok {
		return
	}
	rec.AudioMissing = true
	if err := SaveInstanceRecord(*rec); err != nil {
		flog.Warn("kube create: could not record the missing audio stack", "fleet", c.id.fleet, "instance", c.id.name, "err", err)
	}
	flog.Info("kube create: the instance has no audio stack and no root", "fleet", c.id.fleet, "instance", c.id.name, "cluster", c.id.cluster)
	if audioWanted() {
		c.warn(ctx, AudioUnavailableMessage(c.rec.GetDisplayName()))
	}
}

// noteAudio says in the job's log, before the build, what the inspector's
// check/kubernetes finds of a prebuilt image on a cluster without root: fleet
// gives the audio stack only to images it builds (addAudioLayer), and the
// devcontainer CLI builds none for an image-only configuration without
// features, so the instance has audio only if the image carries the stack.
// Whether it does is known once the pod runs (checkAudio, which warns).
func (c *creation) noteAudio(ctx context.Context, cfg *devcli.Config) {
	if !c.identity.NonRoot || !audioWanted() {
		return
	}
	repo, err := kubecheck.Parse(cfg.Configuration)
	if err != nil {
		return
	}
	note := kubecheck.AudioWarning(repo, kubecheck.Cluster{Name: c.rec.GetDisplayName(), Known: true, NoRoot: true})
	if note == "" {
		return
	}
	_, _ = fmt.Fprintf(backend.JobLogFrom(ctx), "%s (checked once the instance runs)\n", note)
	flog.Info("kube create: a prebuilt image on a cluster without root gets no audio layer", "fleet", c.id.fleet, "instance", c.id.name, "cluster", c.id.cluster, "image", repo.Prebuilt)
}

// audioWanted reports whether either audio direction is on (the daemon's
// global settings); an unreadable config counts as on (the output is on by
// default).
var audioWanted = func() bool {
	cfg, err := state.LoadConfig()
	return err != nil || cfg.MicSettings.Enabled || cfg.OutputSettings.Enabled
}

// audioDetectTimeout bounds the audio stack check at create.
const audioDetectTimeout = time.Minute

// detectAudio runs check (the audio script's own, startup.AudioInstalledScript)
// as the session user (the pod's uid on a cluster without root): whether the
// image carries the audio stack. An error says it could not tell.
func detectAudio(ctx context.Context, s *AgentSession, check string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, audioDetectTimeout)
	defer cancel()
	res, err := s.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", check}}}, nil)
	if err != nil {
		return false, err
	}
	return res.Code == 0, nil
}
