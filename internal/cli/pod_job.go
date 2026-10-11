package cli

import (
	"bufio"
	"encoding/json"
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
	cmd.AddCommand(newPodJobFeedCmd(), newPodJobExportCmd(), newPodJobStatfsCmd())
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

// newPodJobExportCmd is `fleet pod-job ws-export`: a rebuild's export of the
// workspace, as a tar on stdout and its summary last on stderr
// (podseed.ExportWorkspace), run in the instance pod or a tool Job's.
func newPodJobExportCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:           "ws-export",
		Short:         "Write the workspace as a tar to stdout (internal)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mounts, err := podseed.NestedMounts(dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-job ws-export: reading the mounts: %v\n", err)
			}
			out := bufio.NewWriterSize(os.Stdout, 256<<10)
			sum, err := podseed.ExportWorkspace(out, dir, podseed.ExportOptions{Mounts: mounts})
			if err == nil {
				err = out.Flush()
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-job ws-export: %v\n", err)
				return err
			}
			fmt.Fprintln(os.Stderr, podseed.FormatExportSummary(sum))
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "workspace directory")
	_ = cmd.MarkFlagRequired("dir")
	return cmd
}

// newPodJobStatfsCmd is `fleet pod-job statfs`: the size and free room of
// the filesystem holding a path, as JSON (podseed.DiskSpace).
func newPodJobStatfsCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:           "statfs",
		Short:         "Print a filesystem's size and free space (internal)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			disk, err := podseed.DiskUsage(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-job statfs: %v\n", err)
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(disk)
		},
	}
	cmd.Flags().StringVar(&path, "path", podseed.InstallDir, "path on the filesystem")
	return cmd
}
