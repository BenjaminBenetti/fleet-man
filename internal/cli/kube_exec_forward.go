package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// kube_exec_forward.go is `fleet kube-exec forward-stdio` and `fleet
// kube-exec port-forward`, the Kubernetes backend's ForwardStdioCommand and
// PortForwardCommand: one connection to a port in an instance's pod over
// stdin/stdout, or a listener on 127.0.0.1 whose connections go to one. Each
// connection is a stream pair on the pod's port-forward, reached with
// fleetd's credential for the pod (or the kubeconfig while fleetd is away),
// as an attach is. fleetd itself never runs them: it dials pods in-process.

func newKubeExecForwardStdioCmd() *cobra.Command {
	var f kubeExecFlags
	cmd := &cobra.Command{
		Use:           "forward-stdio [flags] <port>",
		Short:         "Connect stdin and stdout to a port in an instance's pod (internal)",
		Hidden:        true,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := forwardPortArg(args[0])
			if err == nil {
				err = runKubeExecForwardStdio(cmd.Context(), f, port, os.Stdin, os.Stdout)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet kube-exec forward-stdio: %v\n", err)
				exitKubeExec(1)
			}
			return nil
		},
	}
	bindKubeExecFlags(cmd, &f)
	return cmd
}

func newKubeExecPortForwardCmd() *cobra.Command {
	var f kubeExecFlags
	cmd := &cobra.Command{
		Use:           "port-forward [flags] <local-port> <remote-port>",
		Short:         "Forward 127.0.0.1:<local-port> to a port in an instance's pod (internal)",
		Hidden:        true,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			local, err := forwardPortArg(args[0])
			if err != nil {
				return err
			}
			remote, err := forwardPortArg(args[1])
			if err != nil {
				return err
			}
			if err := runKubeExecPortForward(cmd.Context(), f, local, remote); err != nil {
				fmt.Fprintf(os.Stderr, "fleet kube-exec port-forward: %v\n", err)
				exitKubeExec(1)
			}
			return nil
		},
	}
	bindKubeExecFlags(cmd, &f)
	return cmd
}

// forwardPortArg is a port argument.
func forwardPortArg(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}

// errNoPod is a forward without the pod to reach.
var errNoPod = errors.New("kube-exec needs --cluster, --namespace and --pod")
