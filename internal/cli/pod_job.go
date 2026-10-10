package cli

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/spf13/cobra"
)

// exitPodJob ends `fleet pod-job` with the job's exit code; a package var so
// tests can catch it.
var exitPodJob = os.Exit

// newPodJobCmd creates the hidden in-pod `fleet pod-job`: the process of a
// fleet Kubernetes Job (seed, rollback, commit), and `pod-job feed`, the pipe
// fleetd feeds it through (see internal/jobboot and internal/podseed).
func newPodJobCmd() *cobra.Command {
	spec := jobboot.Paths{}.Spec()
	cmd := &cobra.Command{
		Use:           "pod-job",
		Short:         "Run a fleet Kubernetes job (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exitPodJob(podseed.RunJob(cmd.Context(), spec, os.Stderr))
			return nil
		},
	}
	cmd.Flags().StringVar(&spec, "spec", spec, "job spec file")
	cmd.AddCommand(newPodJobFeedCmd())
	return cmd
}

func newPodJobFeedCmd() *cobra.Command {
	socket := jobboot.Paths{}.Socket()
	dialTimeout := jobboot.DefaultDialTimeout
	cmd := &cobra.Command{
		Use:           "feed",
		Short:         "Pipe stdin and stdout to a running job's socket (internal)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := jobboot.RunPipe(cmd.Context(), socket, os.Stdin, os.Stdout, dialTimeout)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-job feed: %v\n", err)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&socket, "socket", socket, "job socket")
	cmd.Flags().DurationVar(&dialTimeout, "dial-timeout", dialTimeout, "how long to wait for the socket")
	return cmd
}
