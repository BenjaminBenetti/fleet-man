//go:build !nok8s

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// setup.go runs the devcontainer's lifecycle hooks in an instance pod with
// the devcontainer CLI's own `devcontainer set-up`, through the pod-mode
// docker shim (devcli.SetUp): the shim answers `docker inspect` with the
// container docker would have run (the captured run's labels, environment
// and user, in the instance's pod), and runs every `docker exec` the CLI
// issues on the pod's agent session in the explicit class. set-up keeps the
// reference semantics: the creation hooks once per container (their markers
// on the persisted root compare the container-created time), postStart once
// per run of the dev container (its start time), postAttach every time.

// sessionExecer runs the CLI's execs on an agent session (kubeshim.Execer).
type sessionExecer struct{ s *AgentSession }

// Exec runs req in the explicit class: the user, directory and environment
// the CLI names, the container's own for what it leaves out. It returns when
// the command has ended and its output is written, without waiting for stdin
// (the CLI keeps a shell server's stdin open); a process the command leaves
// in the background keeps running. The command is ended only when ctx ends
// first (the shim asking went away).
func (x sessionExecer) Exec(ctx context.Context, req kubeshim.ExecRequest, stdio kubeshim.ExecStdio) (int, error) {
	env := make(map[string]string, len(req.Env))
	for _, kv := range req.Env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "" {
			env[k] = v
		}
	}
	r := execas.Request{Class: execas.ClassExplicit, Argv: req.Argv, User: req.User, Cwd: req.Dir, Env: env, TTY: req.TTY}
	if req.TTY && req.Size != nil {
		r.Rows, r.Cols = req.Size.Rows, req.Size.Cols
	}
	e, err := x.s.Client().Exec(ctx, podwire.ExecHeader{Request: r})
	if err != nil {
		return -1, err
	}
	stop := context.AfterFunc(ctx, func() { _ = e.Close() })
	defer stop()

	if req.Stdin && stdio.Stdin != nil {
		go func() {
			_, _ = io.Copy(e.Stdin(), stdio.Stdin)
			_ = e.Stdin().Close()
		}()
	} else {
		_ = e.Stdin().Close()
	}
	if req.TTY && stdio.Resize != nil {
		go func() {
			for {
				select {
				case sz := <-stdio.Resize:
					_ = e.Resize(sz.Rows, sz.Cols)
				case <-e.Done():
					return
				}
			}
		}()
	}
	var out sync.WaitGroup
	out.Add(2)
	go func() { defer out.Done(); _, _ = io.Copy(writerOr(stdio.Stdout), e.Stdout()) }()
	go func() { defer out.Done(); _, _ = io.Copy(writerOr(stdio.Stderr), e.Stderr()) }()
	code, err := e.Wait()
	copied := make(chan struct{})
	go func() { out.Wait(); close(copied) }()
	select {
	case <-copied:
	case <-ctx.Done():
		// The shim that asked is gone (or set-up is over): output it no
		// longer reads must not hold the exec, which its caller waits for
		// before it closes the shim's connection that a blocked copy
		// writes to.
		select {
		case <-copied:
		case <-time.After(execOutputGrace):
		}
	}
	if err != nil && ctx.Err() == nil {
		return -1, err
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return code, nil
}

// execOutputGrace bounds how long an ended exec's output is still copied to
// a shim that asked for it once that shim is gone.
const execOutputGrace = 5 * time.Second

func writerOr(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// setUp is one set-up run in an instance pod.
type setUp struct {
	session *AgentSession
	pod     *corev1.Pod
	// container is the captured container; its pod identity is filled in
	// here.
	container   kubeshim.Container
	containerID string
	// created is the instance container's creation time (fleetd's record of
	// it, the StatefulSet's container-created annotation), which set-up's
	// creation markers compare.
	created         time.Time
	workspaceFolder string
	tools           devcli.Tools
}

// run runs set-up: every hook that is due.
func (u setUp) run(ctx context.Context) error {
	c := u.container
	c.ContainerID = u.containerID
	c.Name = u.pod.Name
	c.Created = u.created
	st := containerStatus(u.pod, DevContainer)
	if st == nil || st.State.Running == nil {
		return errors.New("the instance's dev container is not running")
	}
	c.StartedAt = kubeshim.StartedAt(st.State.Running.StartedAt.Time, string(u.pod.UID), st.RestartCount)
	limit := DefaultDeadlines().SetUp
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	_, err := devcli.SetUp(sctx, devcli.SetUpRequest{
		Tools:           u.tools,
		Container:       c,
		WorkspaceFolder: u.workspaceFolder,
		Execer:          sessionExecer{u.session},
		RemoteEnv:       hookRemoteEnv(u.session.Sockets()),
	})
	return setUpError(ctx, limit, err)
}

// setUpWait is what a set-up's deadline error says fleet waited for.
const setUpWait = "devcontainer set-up (the lifecycle hooks)"

// setUpError is a set-up's error as the create, rebuild or start reports it:
// one stopped by its own deadline (FLEET_KUBE_SETUP_TIMEOUT), not by its
// caller's context, is kubernetes:timeout naming what still ran in the pod.
func setUpError(ctx context.Context, limit time.Duration, err error) error {
	var te *devcli.TimeoutError
	if err == nil || ctx.Err() != nil || !errors.As(err, &te) {
		return err
	}
	detail := "the devcontainer CLI was still running (its log is in the creation log)"
	if len(te.Running) > 0 {
		detail = "still running in the pod: " + strings.Join(te.Running, "; ")
	}
	return &DeadlineError{Kind: CodeTimeout, Wait: setUpWait, After: limit, Detail: detail, Env: EnvSetUpTimeout, Err: err}
}

// hookRemoteEnv is fleet's remote environment for the lifecycle hooks: the
// in-pod ssh-agent socket while the agent has it bound (EnsureAgent applied
// that before set-up), as docker's `devcontainer up --remote-env` gets the
// relay's.
func hookRemoteEnv(s podwire.Sockets) []string {
	if !s.SSHAgent {
		return nil
	}
	return []string{agentsock.EnvAuthSock + "=" + agentsock.ContainerSocketPath}
}

// inspectedContainer reads the captured container (inspect.json) from
// generation gen's spec Secret: what set-up's inspect answers with on a
// start, when the capture is long gone from fleetd's memory. The Secret is
// the one the pod itself mounts, so it holds nothing the pod does not.
func (p *Pool) inspectedContainer(ctx context.Context, clusterID, sts string, gen int) (kubeshim.Container, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return kubeshim.Container{}, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return kubeshim.Container{}, err
	}
	name := names.SpecSecret(sts, gen)
	sec, err := cs.CoreV1().Secrets(c.Namespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return kubeshim.Container{}, fmt.Errorf("reading the instance's spec Secret %s: %w", name, err)
	}
	var out kubeshim.Container
	if err := json.Unmarshal(sec.Data[podspec.InspectFile], &out); err != nil {
		return kubeshim.Container{}, fmt.Errorf("the instance's spec Secret %s holds no container: %w", name, err)
	}
	return out, nil
}
