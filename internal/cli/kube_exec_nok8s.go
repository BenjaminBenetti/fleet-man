//go:build nok8s

package cli

import (
	"context"
	"errors"
)

// The -tags nok8s pod build has no Kubernetes client, and never runs the
// daemon-host `fleet kube-exec` shim's cluster-facing modes.
var errNoKubeExec = errors.New("this fleet build has no Kubernetes support")

func runKubeExecDirect(context.Context, kubeExecFlags, []string, bool) (int, error) {
	return -1, errNoKubeExec
}

func runKubeExecLogs(context.Context, kubeExecLogsFlags) error { return errNoKubeExec }

func runKubeExecForwardStdio(context.Context, kubeExecFlags, int) error { return errNoKubeExec }

func runKubeExecPortForward(context.Context, kubeExecFlags, int, int) error { return errNoKubeExec }
