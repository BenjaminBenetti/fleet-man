//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// cmd.go builds the *exec.Cmd seams of the backend — ExecCommand,
// ExecCommandQuiet, Exec, LogsCommand, Logs — as self-contained `fleet
// kube-exec` commands, the daemon-host shim (cmd/fleet runs it). The argv
// names the instance's pod and whom the command runs as, so any consumer
// runs it unchanged: a `docker exec` on a cluster. Daemon mode (no terminal)
// dials fleetd's local-only PodExec; direct mode (a terminal attach) gets a
// live credential from fleetd (IssueKubeExecCredential) and attaches through
// the API server itself, so an attach survives a fleetd restart. Interactive
// work — a TUI pane, `fleet shell` — runs these quoted (internal/shellquote,
// which the clients already use), including the self-contained argv's paths.

// KubeExecVerb is the fleet subcommand the shim's argv starts with.
const KubeExecVerb = "kube-exec"

// shimMode says how the shim runs a command: on the pod's bulk session (a
// folder's tar), and kept out of fleetd's event log (a quiet exec).
type shimMode uint8

const (
	shimBulk shimMode = 1 << iota
	shimQuiet
)

// kubeExecArgv is the self-contained `fleet kube-exec` argv that runs command
// in the instance's pod as the class's user (user and cwd for the explicit
// and root classes). It carries the connection's kubeconfig and context so
// the shim can reach the pod on its own while fleetd is down.
func (b *Backend) kubeExecArgv(class execas.Class, user, cwd string, mode shimMode, command []string) ([]string, error) {
	w, err := b.where("")
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
	argv := []string{exe, KubeExecVerb,
		"--socket", fleetpaths.SocketPath(),
		"--cluster", w.cluster,
		"-n", ns,
		"--pod", names.Pod(w.sts),
		"-c", DevContainer,
		"--class", string(class),
	}
	if mode&shimBulk != 0 {
		argv = append(argv, "--bulk")
	}
	if mode&shimQuiet != 0 {
		argv = append(argv, "--quiet")
	}
	if rec.Context != "" {
		argv = append(argv, "--context", rec.Context)
	}
	for _, p := range rec.KubeconfigPaths {
		argv = append(argv, "--kubeconfig", p)
	}
	if user != "" {
		argv = append(argv, "--user", user)
	}
	if cwd != "" {
		argv = append(argv, "--cwd", cwd)
	}
	return append(argv, append([]string{"--"}, command...)...), nil
}

// clusterRecord is the instance's connection record (for the shim's
// kubeconfig and context).
func (b *Backend) clusterRecord() (fleet.KubeCluster, error) {
	k, err := state.LoadKubeClusters()
	if err != nil {
		return fleet.KubeCluster{}, err
	}
	rec := k.Find(b.inst.KubeCluster)
	if rec == nil {
		return fleet.KubeCluster{}, fmt.Errorf("%w: %s", ErrClusterNotFound, b.inst.KubeCluster)
	}
	return *rec, nil
}

// kubeExecCmd is the *exec.Cmd running kubeExecArgv; an argv it cannot build
// (the record names no connection) becomes a command that fails with the
// reason when started, so a caller sees it rather than a missing program.
func (b *Backend) kubeExecCmd(class execas.Class, user, cwd string, mode shimMode, command []string) *exec.Cmd {
	argv, err := b.kubeExecArgv(class, user, cwd, mode, command)
	if err != nil {
		return failingCmd(err)
	}
	return exec.Command(argv[0], argv[1:]...)
}

// failingCmd is a command that fails with err when started.
func failingCmd(err error) *exec.Cmd {
	return &exec.Cmd{Path: "fleet-kube-exec", Args: []string{"fleet-kube-exec"}, Err: err}
}

// Exec runs command interactively in the pod with the caller's stdio
// inherited (the Backend.Exec contract): a `fleet kube-exec` in direct mode
// when stdin is a terminal, daemon mode otherwise.
func (b *Backend) Exec(workspaceDir string, command []string) error {
	cmd := b.kubeExecCmd(execas.ClassExec, "", "", 0, command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	start := time.Now()
	err := cmd.Run()
	flog.ContainerExec("kubernetes", workspaceDir, command, time.Since(start), err)
	return err
}

// ExecCommand returns an unstarted `fleet kube-exec` command running command
// in the pod as the devcontainer's remote user (class exec), logging one
// timed "container exec" entry when the caller runs it.
func (b *Backend) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(b.kubeExecCmd(execas.ClassExec, "", "", 0, command), func(d time.Duration, err error) {
		flog.ContainerExec("kubernetes", workspaceDir, command, d, err)
	})
}

// ExecCommandQuiet is ExecCommand without the event-log entry, fleetd's
// PodExec one included.
func (b *Backend) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(b.kubeExecCmd(execas.ClassExec, "", "", shimQuiet, command), nil)
}

// FolderExecCommand is ExecCommandQuiet routed to the pod's bulk session: the
// `fleet copy` server handler runs a folder's `tar` through it so a large
// transfer does not hold up the agent's main session (backend.FolderExecer).
func (b *Backend) FolderExecCommand(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(b.kubeExecCmd(execas.ClassExec, "", "", shimBulk|shimQuiet, command), nil)
}

// CopyFile streams src into the pod at remotePath as the remote user,
// atomically, through the pod's agent session: a small file on the agent
// session (FileWrite), a larger one on the bulk session. The parent
// directory is created as the same user. Unlike docker, nothing runs on the
// daemon host.
func (b *Backend) CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error {
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	s, err := b.agent(ctx, false)
	if err != nil {
		return err
	}
	h := podwire.FileWriteHeader{Path: remotePath, Mode: fileMode(mode), Size: -1, Class: execas.ClassExec}
	// Buffer to a host temp so the size is known: the agent session refuses
	// more than podwire.SmallFile up front, and the bulk session takes any
	// size. A file is rarely large, so this stays in memory via a tee only
	// when small; for an unknown-size reader fall to the bulk session.
	buf, size, err := peek(src, podwire.SmallFile)
	if err != nil {
		return err
	}
	if size <= podwire.SmallFile {
		h.Size = size
		return s.Client().WriteFile(ctx, h, buf)
	}
	bulk, err := s.Bulk(ctx)
	if err != nil {
		return fmt.Errorf("the pod's bulk session: %w", err)
	}
	return bulk.WriteFile(ctx, h, buf)
}

// peek reads up to limit+1 bytes of r and returns a reader over all of r
// (what it read, then the rest) and, when the whole input is at most limit,
// its exact size (else limit+1, "larger than the small-file cap").
func peek(r io.Reader, limit int64) (io.Reader, int64, error) {
	head := make([]byte, limit+1)
	n, err := io.ReadFull(r, head)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return bytes.NewReader(head[:n]), int64(n), nil
	case err != nil:
		return nil, 0, err
	}
	return io.MultiReader(bytes.NewReader(head[:n]), r), limit + 1, nil
}

// LogsCommand returns an unstarted `fleet kube-exec logs` command: the saved
// container.log (every earlier run) first, then the live pod stream when the
// instance runs; -f follows the live part.
func (b *Backend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	argv, err := b.logsArgv(containerID, follow)
	if err != nil {
		return failingCmd(err)
	}
	return exec.Command(argv[0], argv[1:]...)
}

// Logs runs LogsCommand with the caller's stdout and stderr inherited.
func (b *Backend) Logs(containerID string, follow bool) error {
	cmd := b.LogsCommand(containerID, follow)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// logsArgv is the `fleet kube-exec logs` argv.
func (b *Backend) logsArgv(containerID string, follow bool) ([]string, error) {
	w, err := b.where(containerID)
	if err != nil {
		return nil, err
	}
	fleetName, err := b.fleet()
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
	argv := []string{exe, KubeExecVerb, "logs",
		"--socket", fleetpaths.SocketPath(),
		"--cluster", w.cluster,
		"-n", ns,
		"--pod", names.Pod(w.sts),
		"-c", DevContainer,
		"--fleet", fleetName,
		"--instance", b.inst.Name,
	}
	if rec.Context != "" {
		argv = append(argv, "--context", rec.Context)
	}
	for _, p := range rec.KubeconfigPaths {
		argv = append(argv, "--kubeconfig", p)
	}
	if follow {
		argv = append(argv, "-f")
	}
	return argv, nil
}

// Stats reports the dev container's CPU and memory from the pod's agent
// (podwire.MethodStats; docker stats' formulas over its cgroup). Instances
// with no live session are left out, so the poller keeps their last reading.
func (b *Backend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	out := make(map[string]*backend.ContainerStats, len(containerIDs))
	for _, id := range containerIDs {
		s := b.liveSession(id)
		if s == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
		var info podwire.StatsInfo
		err := s.Client().Call(ctx, podwire.MethodStats, nil, &info)
		cancel()
		if err != nil {
			continue
		}
		out[id] = &backend.ContainerStats{CPUMillicores: info.CPUMillicores, MemoryMB: float64(info.MemoryBytes) / (1 << 20)}
	}
	return out, nil
}

// EditorURI has no editor integration on a cluster yet (Remote-SSH lands in
// a later unit).
func (b *Backend) EditorURI(string, string) (string, bool) { return "", false }

// PortForwardCommand / ForwardStdioCommand: forwarding on a cluster is the
// port-forward data plane's (a later unit); the handle has none yet.
func (b *Backend) PortForwardCommand(string, int, int) *exec.Cmd {
	return failingCmd(notYet{what: "forwarding a port"})
}
func (b *Backend) ForwardStdioCommand(string, int) (*exec.Cmd, bool) { return nil, false }

// ResolveHostname: a pod has no host-reachable address fleetd dials
// directly.
func (b *Backend) ResolveHostname(string) (string, bool) { return "", false }

// Bounds of the agent work a seam does.
const (
	copyTimeout  = 10 * time.Minute
	statsTimeout = 10 * time.Second
)

// fileMode is a docker-style int mode as an fs.FileMode's permission bits.
func fileMode(mode int) fs.FileMode {
	m := fs.FileMode(mode) & 0o777
	if m == 0 {
		m = 0o644
	}
	return m
}
