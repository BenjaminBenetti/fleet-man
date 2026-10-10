//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// agent.go is the handle's work over the pod's agent session: the session
// polls (tmux sessions, their screens, the agent tool), scripts, the control
// directory's files and the fleet binary's staging. The polls never start a
// session (the supervisor keeps one up in every running instance, and a poll
// that found none would otherwise cost an API server exec upgrade each
// time): without one they report nothing. A script, a control file or a
// staging asks for one, which bootstraps it when it is down.

// Bounds of the agent work.
const (
	// pollTimeout bounds one poll.
	pollTimeout = 10 * time.Second
	// agentWait bounds a script's wait for its pod's agent (a bootstrap has
	// its own deadline, FLEET_KUBE_AGENT_TIMEOUT).
	agentWait = 3 * time.Minute
	// scriptTimeout bounds a script once it runs.
	scriptTimeout = 10 * time.Minute
)

// liveSession is the instance's agent session when one is up; it never
// starts one.
func (b *Backend) liveSession(containerID string) *AgentSession {
	w, err := b.where(containerID)
	if err != nil {
		return nil
	}
	return b.p().Session(PodTarget{Cluster: w.cluster, StatefulSet: w.sts})
}

// agent is the instance's agent session, bootstrapped (and verified against
// the instance record) when none is up; restage stages the daemon's build
// of fleet when the pod runs another.
func (b *Backend) agent(ctx context.Context, restage bool) (*AgentSession, error) {
	t, err := b.PodTarget()
	if err != nil {
		return nil, err
	}
	t.Priority, t.Restage = PriorityInteractive, restage
	s, err := b.p().EnsureAgent(ctx, t)
	return s, classify(err)
}

// script runs a shell script as the instance's session user (the script
// class: `docker exec -u <remoteUser>`), output buffered.
func script(ctx context.Context, s *AgentSession, text string) (*podwire.RunResult, error) {
	return s.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", text}}}, nil)
}

// poll runs a poll script on the instance's live session: its stdout, and
// false when there is no session or the script failed.
func (b *Backend) poll(containerID, text string) (string, bool) {
	s := b.liveSession(containerID)
	if s == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	res, err := script(ctx, s, text)
	if err != nil || res.Code != 0 {
		return "", false
	}
	return string(res.Stdout), true
}

// ListSessions lists the instance's tmux sessions (ListSessionsScript) on
// its live agent session; "" without one.
func (b *Backend) ListSessions(containerID string) string {
	out, _ := b.poll(containerID, backend.ListSessionsScript)
	return out
}

// CaptureAllSessions captures every tmux session's screen in one script.
func (b *Backend) CaptureAllSessions(containerID string) backend.AllSessions {
	out, ok := b.poll(containerID, backend.CaptureAllScript)
	if !ok {
		return backend.AllSessions{OK: false}
	}
	sessions, files, hookMissing := backend.ParseCaptureOutput(out)
	return backend.AllSessions{Sessions: sessions, ExtraFiles: files, HookScriptMissing: hookMissing, OK: true}
}

// AgentToolProbe detects the agent tool running in the instance.
func (b *Backend) AgentToolProbe(containerID string) (string, bool) {
	out, ok := b.poll(containerID, backend.ToolProbeScript)
	if !ok {
		return "", false
	}
	return backend.ParseToolProbeOutput(out)
}

// RunScript runs script as the instance's session user and returns its
// output (stdout, then stderr). A script that exits non-zero returns its
// output and an "exit status N" error, as `docker exec` would.
func (b *Backend) RunScript(containerID, text string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agentWait+scriptTimeout)
	defer cancel()
	s := b.liveSession(containerID)
	if s == nil {
		wait, cancelWait := context.WithTimeout(ctx, agentWait)
		var err error
		s, err = b.agent(wait, false)
		cancelWait()
		if err != nil {
			return "", err
		}
	}
	res, err := script(ctx, s, text)
	if err != nil {
		return "", fmt.Errorf("running a script in the instance: %w", err)
	}
	out := string(res.Stdout) + string(res.Stderr)
	switch {
	case res.Signal != "":
		return out, fmt.Errorf("signal: %s", res.Signal)
	case res.Code != 0:
		return out, fmt.Errorf("exit status %d", res.Code)
	}
	return out, nil
}

// controlPath is a control file's path in the pod; name must be a base name.
func controlPath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("control file name %q is not a base name", name)
	}
	return control.ContainerMountDir + "/" + name, nil
}

// errNoSession is a control file write while the pod has no agent session:
// the caller writes it again later.
var errNoSession = fmt.Errorf("the instance's pod agent is not connected")

// WriteControlFile writes a file of the instance's control directory
// (backend.ControlChannel) through its live agent session, as root (the
// directory is root's), atomically (the agent writes a temp file and renames
// it, which replaces a link planted at the name). Without a session it fails
// and the caller writes it again later.
func (b *Backend) WriteControlFile(name string, data []byte, mode fs.FileMode) error {
	path, err := controlPath(name)
	if err != nil {
		return err
	}
	s := b.liveSession("")
	if s == nil {
		return errNoSession
	}
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	sum := sha256.Sum256(data)
	return s.Client().WriteFile(ctx, podwire.FileWriteHeader{Path: path, Mode: mode, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Class: execas.ClassRoot}, bytes.NewReader(data))
}

// RemoveControlFile removes a file of the instance's control directory.
func (b *Backend) RemoveControlFile(name string) error {
	path, err := controlPath(name)
	if err != nil {
		return err
	}
	s := b.liveSession("")
	if s == nil {
		return errNoSession
	}
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	res, err := s.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: []string{"rm", "-f", "--", path}}}, nil)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("removing %s: exit %d%s", path, res.Code, detail(string(res.Stderr)))
	}
	return nil
}

// StageFleet keeps the instance's fleet binary the daemon's build
// (fleetlaunch.Stager): the seed installed it, pod-init keeps
// /usr/bin/fleet linked to it, and an agent of another build is restaged
// here, at the moments docker's EnsureFresh restages. beforeRefresh runs just
// before a restage.
func (b *Backend) StageFleet(_ string, beforeRefresh func() error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agentWait)
	defer cancel()
	stale := false
	if s := b.liveSession(""); s != nil {
		stale = s.Stale()
	}
	if stale && beforeRefresh != nil {
		if err := beforeRefresh(); err != nil {
			return false, err
		}
	}
	s, err := b.agent(ctx, true)
	if err != nil {
		return false, err
	}
	return stale && !s.Stale(), nil
}
