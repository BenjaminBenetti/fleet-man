package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetcopy"
	"github.com/spf13/cobra"
)

// newCopyCmd creates the `fleet copy` command (aliased `cp`; inside an instance
// the staged fleet.rc also provides `fc`). It is a generic, scp-style copy: each
// of the two arguments is an endpoint — `[fleet/]instance:path` (a file inside an
// instance), a plain path (a file on the orchestrating machine), or `:path` (the
// current instance) — and the direction is inferred from which side is which. The
// same command copies out of, into, and between instances.
//
// Where the copy is performed depends only on where the command runs, not on the
// direction: a host invocation drives the fleet server directly; an in-instance
// invocation delegates to the connected fleet TUI (the container can't reach the
// host fleetd over gRPC), which runs the very same copy against the host fleet.
// The HELP text is tailored to where the command runs.
func newCopyCmd() *cobra.Command {
	use := "copy <src> <dst>"
	short := "Copy a file to, from, or between fleet instances, scp-style"
	long := "Copy a single file between your machine and a fleet instance — or between\n" +
		"two instances — scp-style. Each path is either `[fleet/]instance:path` (a file\n" +
		"inside an instance, reachable even on a remote fleet) or a plain local path;\n" +
		"the direction follows which side is which:\n\n" +
		"  fleet copy alpha:bin/tool ./tool         # download out of an instance\n" +
		"  fleet copy ./tool alpha:/usr/local/bin/  # upload into an instance\n" +
		"  fleet copy alpha:a beta:b                # copy between two instances\n\n" +
		"A relative instance path resolves against the workspace folder; a directory\n" +
		"destination keeps the source's name. Given a single instance source the file\n" +
		"downloads to the current directory."
	if fleetcopy.InInstance() {
		short = "Copy a file to or from an instance via your fleet TUI (shorthand: fc)"
		long = "Copy a single file between your machine and an instance — or between two\n" +
			"instances — scp-style. The copy is performed by the fleet TUI on your\n" +
			"machine, so it works even when this instance lives on a remote server.\n\n" +
			"A plain path is a file on your machine (where the TUI runs); `:path` is a\n" +
			"file in THIS instance; `[fleet/]instance:path` is any instance in your fleet:\n\n" +
			"  fc :bin/tool ~/Downloads/   # copy this instance's file to your machine\n" +
			"  fc report.csv :/tmp/        # copy a file from your machine into this instance\n" +
			"  fc :out.bin other:/tmp/     # copy this instance's file into another instance\n\n" +
			"Given a single `:`/instance source the file lands in your downloads folder.\n" +
			"Needs the fleet TUI connected on the other end."
	}

	return &cobra.Command{
		Use:     use,
		Aliases: []string{"cp"},
		Short:   short,
		Long:    long,
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := ""
			if len(args) == 2 {
				dst = args[1]
			}
			return runCopy(cmd.Context(), cmd.OutOrStdout(), args[0], dst)
		},
	}
}

// runCopy validates the arguments and routes the copy: in-instance invocations
// delegate to the connected host TUI; host invocations drive the fleet server
// directly. The single-argument form is a download shorthand, so a lone source
// must name an instance — a lone local path has no destination.
func runCopy(ctx context.Context, out io.Writer, src, dst string) error {
	if dst == "" && fleetclient.ParseCopyEndpoint(src).Kind == fleetclient.CopyLocal {
		return fmt.Errorf("a destination is required: %q is a local file, so name where it should go (e.g. `fleet copy %s instance:/path`)", src, src)
	}
	if fleetcopy.InInstance() {
		// The container can't reach the host fleetd over gRPC; ask the connected
		// TUI to perform the copy against the host fleet (its disk is "local").
		return fleetcopy.Request(fleetcopy.Config{}, out, src, dst)
	}
	return hostCopy(ctx, out, src, dst)
}

// hostCopy resolves both endpoints against the host fleet and runs the generic
// copy engine over the dialled fleet server, with the CLI's own disk as local.
func hostCopy(ctx context.Context, out io.Writer, src, dst string) error {
	srcEnd, err := resolveHostEndpoint(src)
	if err != nil {
		return err
	}
	dstEnd, err := resolveHostEndpoint(dst)
	if err != nil {
		return err
	}
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	res, err := fleetclient.Copy(ctx, conn.Service(), srcEnd, dstEnd, fleetclient.HostLocalPolicy{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Copied %s -> %s (%d bytes)\n", src, res.DestPath, res.Written)
	return nil
}

// resolveHostEndpoint turns a typed argument into a ResolvedEndpoint for the host
// form: a local path stays local, an instance reference resolves its fleet (a
// bare instance infers it from the cwd, like the rest of the CLI), and `:path`
// (self) is rejected — there is no current instance when run on a host.
func resolveHostEndpoint(arg string) (fleetclient.ResolvedEndpoint, error) {
	ep := fleetclient.ParseCopyEndpoint(arg)
	switch ep.Kind {
	case fleetclient.CopyLocal:
		return fleetclient.ResolvedEndpoint{Local: true, Path: ep.Path}, nil
	case fleetclient.CopySelf:
		return fleetclient.ResolvedEndpoint{}, fmt.Errorf("`:path` means the current instance and only works inside one — name the fleet/instance instead (got %q)", arg)
	default:
		if ep.Fleet != "" {
			return fleetclient.ResolvedEndpoint{Fleet: ep.Fleet, Instance: ep.Instance, Path: ep.Path}, nil
		}
		target, err := fleet.Resolve(ep.Instance, "")
		if err != nil {
			return fleetclient.ResolvedEndpoint{}, err
		}
		return fleetclient.ResolvedEndpoint{Fleet: target.Fleet, Instance: target.Instance, Path: ep.Path}, nil
	}
}
