package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetcopy"
	"github.com/spf13/cobra"
)

// newCopyCmd creates the `fleet copy` command (aliased `cp`; inside an instance
// the staged fleet.rc also provides `fc`). It copies a single file out of an
// instance and has two forms, both backed by the server's CopyFile stream:
//
//   - `fleet copy [fleet/]instance:path [dest]` — run anywhere the fleet server
//     is reachable (including a remote one over `fleet remote`): pull the file
//     and write it to dest (default: same basename in the current directory).
//   - `fleet copy <path>` — run INSIDE an instance (shorthand: `fc <path>`):
//     like `fleet launch`, it messages the host over the control socket; the
//     connected fleet TUI then pulls the file and drops it in its local
//     downloads folder. This is the remote-deployment workflow: build in the
//     instance, type `fc bin/tool`, and the binary lands on your own machine.
func newCopyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "copy <[fleet/]instance:path | path> [dest]",
		Aliases: []string{"cp"},
		Short:   "Copy a file out of an instance (in-instance shorthand: fc)",
		Long: "Copy a single file out of a fleet instance, scp-style.\n\n" +
			"From the host (or any machine connected to the fleet server, including a remote\n" +
			"one): `fleet copy [fleet/]instance:path [dest]` pulls the file and writes it to\n" +
			"dest — default: same name in the current directory; a relative source path\n" +
			"resolves against the instance's workspace folder.\n\n" +
			"From inside an instance: `fleet copy <path>` (or the `fc` alias) asks the\n" +
			"connected host fleet TUI to copy the file out; it lands in the host user's\n" +
			"downloads folder. Handy for completely remote deployments — build inside the\n" +
			"instance, `fc` the binary, and it arrives on your local machine.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if target, srcPath, ok := splitCopySource(args[0]); ok {
				dest := ""
				if len(args) == 2 {
					dest = args[1]
				}
				return copyFromInstance(cmd.Context(), cmd.OutOrStdout(), target, srcPath, dest)
			}
			if len(args) == 2 {
				return fmt.Errorf("the in-instance form takes no destination — the file lands in the host's downloads folder (use `fleet copy [fleet/]instance:path [dest]` to choose one)")
			}
			// The in-instance form: like `fleet launch`, message the host over
			// the control socket; the connected TUI pulls the file.
			return fleetcopy.Request(fleetcopy.Config{}, cmd.OutOrStdout(), args[0])
		},
	}
}

// splitCopySource splits a "[fleet/]instance:path" source argument; ok is false
// when the argument is a plain local path (the in-instance form). Mirrors scp's
// disambiguation rule: anything starting with /, . or ~ is always a path, so a
// local filename containing a colon can be spelled unambiguously.
func splitCopySource(arg string) (target, path string, ok bool) {
	i := strings.Index(arg, ":")
	if i <= 0 {
		return "", "", false
	}
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return "", "", false
	}
	// The instance reference is "instance" or "fleet/instance" — nothing more.
	prefix := arg[:i]
	parts := strings.Split(prefix, "/")
	if len(parts) > 2 || slices.Contains(parts, "") {
		return "", "", false
	}
	if arg[i+1:] == "" {
		return "", "", false
	}
	return prefix, arg[i+1:], true
}

// copyFromInstance is the host-side form: pull fleet/instance:srcPath through
// the server's CopyFile stream and write it to dest locally.
func copyFromInstance(ctx context.Context, out io.Writer, targetName, srcPath, dest string) error {
	target, err := fleet.Resolve(targetName, "")
	if err != nil {
		return err
	}
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	destPath, written, err := fleetclient.CopyFileTo(ctx, conn.Service(), target.Fleet, target.Instance, srcPath, dest)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Copied %s/%s:%s -> %s (%d bytes)\n", target.Fleet, target.Instance, srcPath, destPath, written)
	return nil
}
