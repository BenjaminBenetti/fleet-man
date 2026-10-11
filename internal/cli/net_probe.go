package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/BenjaminBenetti/fleet-man/internal/netprobe"
)

// newNetProbeCmd creates the hidden `fleet net-probe` command: it prints the
// first URL that accepts a connection, else DIRECT. An instance on a cluster
// runs it through the staged binary as apt's proxy detector
// (debcache.ProxyDetector), so apt uses the fleet's deb cache only while it
// answers; on the host it is meaningless.
func newNetProbeCmd() *cobra.Command {
	timeout := netprobe.DefaultTimeout
	cmd := &cobra.Command{
		Use:          "net-probe <url>...",
		Short:        "Print the first URL that answers, else DIRECT (internal)",
		Hidden:       true,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			got, err := netprobe.First(cmd.Context(), args, timeout)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), got)
			return err
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "connect timeout per URL")
	return cmd
}
