package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/BenjaminBenetti/fleet-man/internal/execstream"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// kube_exec.go is `fleet kube-exec`, the daemon-host shim the Kubernetes
// backend's *exec.Cmd seams hand out: a self-contained argv that runs one
// command in an instance's pod, so a TUI pane, `fleet shell` or a server-side
// ExecCommand reaches a cluster the way they reach a container. Hidden: it is
// fleet's own plumbing, not a user command.
//
// Daemon mode (stdin is not a terminal) dials fleetd's local socket and runs
// the command through the local-only PodExec RPC on the pod's agent session.
// Direct mode (a terminal attach) gets the live transport fleetd itself uses
// from fleetd (IssueKubeExecCredential), attaches through the API server with
// it, and re-attaches after a dropped connection, so a pane survives an API
// server rollout or a fleetd restart; when the pod itself was replaced the
// attach exits 75 and the caller's wrapper (`tmux new-session -A`) rejoins a
// fresh session. Raw mode runs the command with the pod's own shell when it
// has no fleet binary yet. The client-go modes (direct, raw, logs) are in the
// -tags nok8s-excluded file; the pod build never runs the shim.

type kubeExecFlags struct {
	socket     string
	cluster    string
	context    string
	kubeconfig []string
	namespace  string
	pod        string
	container  string
	class      string
	user       string
	cwd        string
	raw        bool
	bulk       bool
}

func newKubeExecCmd() *cobra.Command {
	var f kubeExecFlags
	cmd := &cobra.Command{
		Use:           "kube-exec [flags] -- command...",
		Short:         "Run a command in a Kubernetes instance's pod (internal)",
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.cluster == "" || f.pod == "" {
				return errors.New("kube-exec needs --cluster and --pod")
			}
			if len(args) == 0 {
				return errors.New("kube-exec needs a command after --")
			}
			code, err := runKubeExec(cmd.Context(), f, args)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet kube-exec: %v\n", err)
			}
			exitKubeExec(code)
			return nil
		},
	}
	bindKubeExecFlags(cmd, &f)
	cmd.Flags().BoolVar(&f.raw, "raw", false, "run with the pod's own shell (no fleet binary)")
	cmd.Flags().BoolVar(&f.bulk, "bulk", false, "run on the pod's bulk session (a folder tar)")
	cmd.AddCommand(newKubeExecLogsCmd())
	return cmd
}

func bindKubeExecFlags(cmd *cobra.Command, f *kubeExecFlags) {
	cmd.Flags().StringVar(&f.socket, "socket", "", "fleetd's unix socket")
	cmd.Flags().StringVar(&f.cluster, "cluster", "", "the connection id")
	cmd.Flags().StringVar(&f.context, "context", "", "the kubeconfig context (fleetd-down fallback)")
	cmd.Flags().StringArrayVar(&f.kubeconfig, "kubeconfig", nil, "kubeconfig path (fleetd-down fallback)")
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "", "the pod's namespace")
	cmd.Flags().StringVar(&f.pod, "pod", "", "the instance's pod")
	cmd.Flags().StringVarP(&f.container, "container", "c", "", "the pod's container")
	cmd.Flags().StringVar(&f.class, "class", "exec", "exec identity: exec, script, root or explicit")
	cmd.Flags().StringVar(&f.user, "user", "", "the user (explicit/root class)")
	cmd.Flags().StringVar(&f.cwd, "cwd", "", "the working directory (explicit class)")
}

type kubeExecLogsFlags struct {
	kubeExecFlags
	fleet    string
	instance string
	follow   bool
}

func newKubeExecLogsCmd() *cobra.Command {
	var f kubeExecLogsFlags
	cmd := &cobra.Command{
		Use:           "logs [flags]",
		Short:         "Print an instance's saved and live container logs (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runKubeExecLogs(cmd.Context(), f); err != nil {
				fmt.Fprintf(os.Stderr, "fleet kube-exec logs: %v\n", err)
				return err
			}
			return nil
		},
	}
	bindKubeExecFlags(cmd, &f.kubeExecFlags)
	cmd.Flags().StringVar(&f.fleet, "fleet", "", "the instance's fleet (for the saved log)")
	cmd.Flags().StringVar(&f.instance, "instance", "", "the instance (for the saved log)")
	cmd.Flags().BoolVarP(&f.follow, "follow", "f", false, "follow the live log")
	return cmd
}

// exitKubeExec is os.Exit; a var so tests can catch it.
var exitKubeExec = os.Exit

// winchSignal is SIGWINCH; signalNotify is signal.Notify (both vars so tests
// can drive resizes).
var (
	winchSignal  os.Signal = syscall.SIGWINCH
	signalNotify           = signal.Notify
)

// runKubeExec picks the mode and runs the command, returning its exit code.
func runKubeExec(ctx context.Context, f kubeExecFlags, argv []string) (int, error) {
	tty := term.IsTerminal(os.Stdin.Fd())
	if tty || f.raw {
		return runKubeExecDirect(ctx, f, argv, tty)
	}
	return runKubeExecDaemon(ctx, f, argv)
}

// runKubeExecDaemon runs the command through fleetd's PodExec RPC on the
// local socket: the command without a terminal, on the pod's agent session.
func runKubeExecDaemon(ctx context.Context, f kubeExecFlags, argv []string) (int, error) {
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return -1, fmt.Errorf("reaching fleetd: %w", err)
	}
	defer conn.Close()
	return execstream.Run(ctx, conn.Service(), execstream.Options{
		Pod: &execstream.PodExecTarget{
			Cluster: f.cluster, Namespace: f.namespace, Pod: f.pod, Container: f.container,
			Class: f.class, User: f.user, Cwd: f.cwd, Bulk: f.bulk,
		},
		Argv:   argv,
		TTY:    false,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}

// kubeExecSize is the terminal's current size (0,0 when not a terminal).
func kubeExecSize() (rows, cols uint16) {
	c, r, err := term.GetSize(os.Stdin.Fd())
	if err != nil || c <= 0 || r <= 0 {
		return 0, 0
	}
	return uint16(r), uint16(c)
}

// kubeExecRawTerminal puts stdin into raw mode and returns a func that
// restores it; nil when stdin is not a terminal.
func kubeExecRawTerminal() func() {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		return nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil
	}
	return func() { _ = term.Restore(fd, state) }
}
