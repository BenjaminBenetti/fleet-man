package cli

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/spf13/cobra"
)

// exitExecAs ends `fleet exec-as` with its command's would-be exit code; a
// package var so tests can catch it.
var exitExecAs = os.Exit

// newExecAsCmd creates the hidden in-pod `fleet exec-as`: run the request
// framed on stdin as its pod user, in place of this process. fleetd kube-execs
// it while a pod's agent session is down (see internal/execas).
func newExecAsCmd() *cobra.Command {
	opts := execas.ExecAsOptions{SpecPath: execas.DefaultSpecPath, CacheDir: execas.DefaultCacheDir}
	cmd := &cobra.Command{
		Use:           "exec-as",
		Short:         "Run the exec request framed on stdin as its pod user (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// On success the command has replaced this process.
			err := execas.RunExecAs(cmd.Context(), os.Stdin, opts)
			fmt.Fprintf(os.Stderr, "fleet exec-as: %v\n", err)
			exitExecAs(execas.ExitCode(err))
			return err
		},
	}
	cmd.Flags().StringVar(&opts.SpecPath, "spec", opts.SpecPath, "exec spec file")
	cmd.Flags().StringVar(&opts.CacheDir, "cache-dir", opts.CacheDir, "env probe cache dir (empty: none)")
	return cmd
}
