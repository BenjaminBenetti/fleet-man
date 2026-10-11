//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// forward.go is the handle's way into the instance's ports. fleetd dials them
// itself (backend.PortDialer): one stream pair on the pod's pooled
// port-forward connection (pfpool.go), or, where the cluster forbids
// pods/portforward, a forward stream on the pod's agent session, which also
// carries the forwards that need a true half-close (the cluster's runtime ends
// a port-forward about a second after the local EOF). The *exec.Cmd forms
// (ForwardStdioCommand, PortForwardCommand) are the daemon-host shim's, for
// callers outside fleetd.

var (
	_ backend.PortDialer      = (*Backend)(nil)
	_ backend.HalfCloseDialer = (*Backend)(nil)
	_ backend.PortWarmer      = (*Backend)(nil)
)

// DialPort connects to port on the pod's loopback: a stream pair on the pod's
// pooled port-forward connection, or the agent's forward stream when the
// cluster forbids port-forwards.
func (b *Backend) DialPort(ctx context.Context, containerID string, port int) (io.ReadWriteCloser, error) {
	t, err := b.forwardTarget(containerID)
	if err != nil {
		return nil, err
	}
	conn, err := b.p().DialPort(ctx, t, port)
	if errors.Is(err, kubeexec.ErrPortForwardForbidden) {
		return b.dialAgentPort(ctx, port)
	}
	if err != nil {
		return nil, classify(err)
	}
	return conn, nil
}

// DialPortHalfClose connects to port through the pod agent, whose forward
// keeps reading after the half-close (backend.HalfCloseDialer).
func (b *Backend) DialPortHalfClose(ctx context.Context, containerID string, port int) (io.ReadWriteCloser, error) {
	if _, err := b.forwardTarget(containerID); err != nil {
		return nil, err
	}
	return b.dialAgentPort(ctx, port)
}

// WarmPorts opens the pod's port-forward connection ahead of the first dial
// (backend.PortWarmer); where the cluster forbids port-forwards it brings up
// the agent session instead.
func (b *Backend) WarmPorts(ctx context.Context, containerID string) error {
	t, err := b.forwardTarget(containerID)
	if err != nil {
		return err
	}
	err = b.p().WarmPorts(ctx, t)
	if errors.Is(err, kubeexec.ErrPortForwardForbidden) {
		_, err = b.agent(ctx, false)
	}
	return classify(err)
}

// forwardTarget is the instance's pod, for a containerID that names it.
func (b *Backend) forwardTarget(containerID string) (PodTarget, error) {
	if containerID != "" {
		if _, err := b.where(containerID); err != nil {
			return PodTarget{}, err
		}
	}
	return b.PodTarget()
}

// dialAgentPort connects to port through the pod's agent session (TagForward),
// bootstrapping one when none is up.
func (b *Backend) dialAgentPort(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	wait, cancel := context.WithTimeout(ctx, agentWait)
	defer cancel()
	s, err := b.agent(wait, false)
	if err != nil {
		return nil, err
	}
	conn, err := s.Client().Forward(wait, port)
	if err != nil {
		return nil, fmt.Errorf("through the pod agent: %w", err)
	}
	return conn, nil
}

// ForwardStdioCommand is a `fleet kube-exec forward-stdio` command: its stdin
// and stdout are one connection to port in the pod (a stream pair of its own
// port-forward), and closing its stdin half-closes it. fleetd itself dials
// (DialPort); this is the form for a process outside it.
func (b *Backend) ForwardStdioCommand(containerID string, remotePort int) (*exec.Cmd, bool) {
	argv, err := b.shimForwardArgv(containerID, "forward-stdio", strconv.Itoa(remotePort))
	if err != nil {
		return failingCmd(err), true
	}
	return exec.Command(argv[0], argv[1:]...), true
}

// PortForwardCommand is a `fleet kube-exec port-forward` command listening
// on the daemon host's 127.0.0.1:localPort, each connection a stream pair to
// remotePort in the pod.
func (b *Backend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	argv, err := b.shimForwardArgv(containerID, "port-forward", strconv.Itoa(localPort), strconv.Itoa(remotePort))
	if err != nil {
		return failingCmd(err)
	}
	return exec.Command(argv[0], argv[1:]...)
}

// shimForwardArgv is the self-contained `fleet kube-exec <sub>` argv of a
// forwarding subcommand: the pod, and how to reach it with or without fleetd.
func (b *Backend) shimForwardArgv(containerID, sub string, args ...string) ([]string, error) {
	w, err := b.where(containerID)
	if err != nil {
		return nil, err
	}
	exe, err := fleetExecutable()
	if err != nil {
		return nil, fmt.Errorf("the fleet binary: %w", err)
	}
	rec, _ := b.clusterRecord()
	ns := w.namespace
	if ns == "" {
		ns = rec.Namespace
	}
	argv := []string{exe, KubeExecVerb, sub,
		"--socket", fleetpaths.SocketPath(),
		"--cluster", w.cluster,
		"-n", ns,
		"--pod", names.Pod(w.sts),
	}
	if rec.Context != "" {
		argv = append(argv, "--context", rec.Context)
	}
	for _, p := range rec.KubeconfigPaths {
		argv = append(argv, "--kubeconfig", p)
	}
	return append(argv, args...), nil
}
