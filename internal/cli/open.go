package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetcopy"
	"github.com/BenjaminBenetti/fleet-man/internal/platform"
	"github.com/spf13/cobra"
)

// newOpenCmd creates `fleet open` (inside an instance the staged fleet.rc also
// provides `fo`): a `fleet copy` to the user's machine followed by opening the
// delivered file with that machine's default application. It short-circuits
// the "fc the screenshot, then xdg-open it" two-step. The endpoint grammar is
// copy's; the source must be in an instance and the destination, when given,
// on the user's machine.
//
// As with copy, WHERE the copy and the open happen depends only on where the
// command runs: a host invocation copies via the fleet server and opens the file
// itself; an in-instance invocation delegates both to the connected fleet TUI
// (the container can reach neither the host fleetd nor the human's desktop),
// which prompts for confirmation like any host-touching fc.
func newOpenCmd() *cobra.Command {
	use := "open <src> [dst]"
	short := "Copy a file from an instance and open it"
	long := "Copy a file (or directory) out of an instance to your machine, then open it\n" +
		"with your desktop's default application. The endpoints are fleet copy's.\n\n" +
		"  fleet open alpha:out/chart.png             copy to the cwd and open\n" +
		"  fleet open alpha:report.pdf ~/Documents/   copy there and open\n\n" +
		"FLEET_OPENER overrides the opener (program + args, split on whitespace;\n" +
		"default: xdg-open, open, or wslview). Executables are never opened."
	if fleetcopy.InInstance() {
		long = "Copy a file (or directory) from this instance to your machine, then open it\n" +
			"there with your desktop's default application. The endpoints are fc's.\n\n" +
			"  fo ./out/chart.png                copy to your downloads folder and open\n" +
			"  fo report.pdf host:~/Documents/   copy there and open\n\n" +
			"The copy asks for confirmation in the fleet TUI, which does the opening\n" +
			"(FLEET_OPENER in its environment overrides the opener). Executables are\n" +
			"never opened."
	}

	return &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 2 {
				dst = args[1]
			}
			return runOpen(cmd.Context(), cmd.OutOrStdout(), args[0], dst)
		},
	}
}

// runOpen routes like runCopy: in-instance invocations rewrite plain (this-
// instance) paths and delegate the copy + open to the host TUI; host invocations
// copy via the fleet server and open the delivered file locally.
func runOpen(ctx context.Context, out io.Writer, src, dst string) error {
	if fleetcopy.InInstance() {
		src = rewriteInstanceLocal(src)
		dst = rewriteInstanceLocal(dst)
		if err := requireOpenEndpoints(src, dst); err != nil {
			return err
		}
		return fleetcopy.Request(fleetcopy.Config{}, out, src, dst, true)
	}
	if err := requireOpenEndpoints(src, dst); err != nil {
		return err
	}
	res, err := hostCopy(ctx, src, dst)
	if err != nil {
		return err
	}
	reportCopied(out, src, res)
	opened, err := platform.OpenFile(res.DestPath) // its errors name the path
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Opened %s\n", opened)
	return nil
}

// requireOpenEndpoints enforces open's shape: the source must live in an
// instance (a file already on your machine has nothing to fetch — just open
// it), and the destination, when given, must be on your machine — that is the
// only place the file can be opened. Inside an instance a plain dst has already
// been rewritten to a `:` self endpoint, so the error steers to `host:`.
func requireOpenEndpoints(src, dst string) error {
	switch fleetclient.ParseCopyEndpoint(src).Kind {
	case fleetclient.CopyLocal, fleetclient.CopyHost:
		return fmt.Errorf("source must be in an instance: %q is on your machine", src)
	}
	if dst == "" {
		return nil
	}
	switch fleetclient.ParseCopyEndpoint(dst).Kind {
	case fleetclient.CopyLocal, fleetclient.CopyHost:
		return nil
	}
	return fmt.Errorf("destination must be on your machine (host:path): %q is in an instance", dst)
}
