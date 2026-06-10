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
// instance, scp-style, in one of two forms picked per argument:
//
//   - `fleet copy [fleet/]instance:path [dest]` pulls the file from any
//     reachable fleet server (including a remote one) and writes it locally.
//   - `fleet copy <path> [dest]` — the in-instance form: like `fleet launch`,
//     it messages the host over the control socket; the connected fleet TUI
//     pulls the file to the user's machine — downloads folder by default, dest
//     there when given.
//
// Both forms work from anywhere (a dev instance may run its own nested fleet,
// so the instance:path form must keep working inside one), but the HELP text is
// built for where the command runs: telling a user who is already inside an
// instance to name a fleet/instance makes no sense, and vice versa.
func newCopyCmd() *cobra.Command {
	use := "copy <[fleet/]instance:path> [dest]"
	short := "Copy a file out of an instance"
	long := "Copy a single file out of a fleet instance, scp-style: pull\n" +
		"`[fleet/]instance:path` through the fleet server (including a remote one) and\n" +
		"write it to dest — default: same name in the current directory; a directory\n" +
		"dest keeps the file's name. A relative source path resolves against the\n" +
		"instance's workspace folder.\n\n" +
		"Inside an instance the same command (shorthand: `fc`) takes plain paths and\n" +
		"delivers to your machine via the connected fleet TUI."
	if fleetcopy.InInstance() {
		use = "copy <path> [dest]"
		short = "Copy a file from this instance to your machine (shorthand: fc)"
		long = "Copy a single file out of this instance to the machine your fleet TUI runs\n" +
			"on — even when this instance lives on a remote server.\n\n" +
			"With no destination the file lands in your downloads folder. With one, it\n" +
			"lands there instead: an absolute path is used as-is, `~/` and relative paths\n" +
			"resolve against your home directory, and a directory keeps the file's name.\n" +
			"Needs the fleet TUI connected on the other end."
	}

	return &cobra.Command{
		Use:     use,
		Aliases: []string{"cp"},
		Short:   short,
		Long:    long,
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := ""
			if len(args) == 2 {
				dest = args[1]
			}
			if target, srcPath, ok := splitCopySource(args[0]); ok {
				return copyFromInstance(cmd.Context(), cmd.OutOrStdout(), target, srcPath, dest)
			}
			// The in-instance form: like `fleet launch`, message the host over
			// the control socket; the connected TUI pulls the file — to dest on
			// the user's machine when given, their downloads folder otherwise.
			return fleetcopy.Request(fleetcopy.Config{}, cmd.OutOrStdout(), args[0], dest)
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
