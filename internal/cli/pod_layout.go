package cli

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/spf13/cobra"
)

// newPodLayoutCmd creates the hidden in-pod `fleet pod-layout`: the
// share-layout init container of a Kubernetes instance pod, which lays out
// the fleet's share before the dev container mounts its folders (see
// internal/podshare).
func newPodLayoutCmd() *cobra.Command {
	spec, tree := podshare.LayoutPath, podshare.InitDir
	cmd := &cobra.Command{
		Use:           "pod-layout",
		Short:         "Lay out the fleet's share in an instance pod (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			if err := podshare.RunLayout(spec, tree, jobboot.DefaultTerminationLog); err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-layout: %v\n", err)
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&spec, "spec", spec, "layout file")
	cmd.Flags().StringVar(&tree, "tree", tree, "the share's folder tree")
	return cmd
}
