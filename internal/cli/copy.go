package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetcopy"
	"github.com/spf13/cobra"
)

// newCopyCmd creates the `fleet copy` command (aliased `cp`; inside an instance
// the staged fleet.rc also provides `fc`). It is a generic, scp-style copy: each
// of the two arguments is an endpoint and the direction is inferred from which
// side is which. A plain path is local to the machine the command runs on
// (cwd-relative): the host for `fleet copy`, THIS instance for in-instance `fc`.
// `host:path` is the host machine, and `[fleet/]instance:path` is a named
// instance. The same command copies out of, into, and between instances.
//
// Where the copy is performed depends only on where the command runs, not on the
// direction: a host invocation drives the fleet server directly; an in-instance
// invocation delegates to the connected fleet TUI (the container can't reach the
// host fleetd over gRPC), which runs the very same copy against the host fleet.
// The HELP text is tailored to where the command runs.
func newCopyCmd() *cobra.Command {
	use := "copy <src> <dst>"
	short := "Copy files to, from, and between instances"
	long := "Copy a file or directory between your machine and an instance, or between two\n" +
		"instances. A plain path is local (cwd-relative); [fleet/]instance:path is a\n" +
		"file inside an instance. Direction follows which side is which.\n\n" +
		"  fleet copy alpha:bin/tool ./tool         download from an instance\n" +
		"  fleet copy ./tool alpha:/usr/local/bin/  upload to an instance\n" +
		"  fleet copy alpha:a beta:b                between two instances\n\n" +
		"A relative instance path is relative to the workspace folder; a directory\n" +
		"destination keeps the source name. A lone instance source downloads to the cwd."
	if fleetcopy.InInstance() {
		long = "Copy a file or directory between this instance and your machine, or another\n" +
			"instance. A plain path is in this instance (cwd-relative); host:path is on\n" +
			"your machine; [fleet/]instance:path is any instance in your fleet.\n\n" +
			"  fc ./build/out host:~/Downloads/  this instance -> your machine\n" +
			"  fc host:report.csv /tmp/          your machine  -> this instance\n" +
			"  fc ./out.bin other:/tmp/          this instance -> another instance\n\n" +
			"A lone source downloads to your downloads folder. A copy that touches your\n" +
			"machine asks for confirmation in the fleet TUI."
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

// runCopy routes the copy: in-instance invocations rewrite their plain (this-
// instance) paths to absolute self endpoints and delegate to the connected host
// TUI; host invocations drive the fleet server directly. The single-argument
// form is a download shorthand, so a lone source must be a remote (instance)
// endpoint — a lone path on your own machine has nowhere to go.
func runCopy(ctx context.Context, out io.Writer, src, dst string) error {
	if fleetcopy.InInstance() {
		// A plain path typed inside an instance is THAT instance's file, relative
		// to the cwd; resolve it here (only this process knows its cwd) so the TUI
		// reads/writes the instance, not the host.
		src = rewriteInstanceLocal(src)
		dst = rewriteInstanceLocal(dst)
		if err := requireDownloadSource(src, dst); err != nil {
			return err
		}
		// The container can't reach the host fleetd over gRPC; ask the connected
		// TUI to perform the copy against the host fleet.
		return fleetcopy.Request(fleetcopy.Config{}, out, src, dst, false)
	}
	if err := requireDownloadSource(src, dst); err != nil {
		return err
	}
	res, err := hostCopy(ctx, src, dst)
	if err != nil {
		return err
	}
	reportCopied(out, src, res)
	return nil
}

// reportCopied prints the one-line outcome of a host-form copy (shared with
// `fleet open`, which copies the same way before opening).
func reportCopied(out io.Writer, src string, res fleetclient.CopyResult) {
	fmt.Fprintf(out, "Copied %s -> %s (%d bytes)\n", src, res.DestPath, res.Written)
}

// rewriteInstanceLocal turns a plain (cwd-relative) path typed inside an instance
// into a `:absolute` self endpoint, so the host TUI resolves it against THIS
// instance rather than the host's disk. host:/instance:/: forms (and the empty
// 1-arg dst) pass through unchanged.
func rewriteInstanceLocal(arg string) string {
	if arg == "" || fleetclient.ParseCopyEndpoint(arg).Kind != fleetclient.CopyLocal {
		return arg
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return arg // best-effort; the TUI surfaces an unresolvable path as an error
	}
	return ":" + abs
}

// requireDownloadSource enforces the 1-arg download shorthand: with no
// destination the file is delivered to your machine's downloads folder, so the
// source must be a remote (instance) endpoint — a path on your own machine
// (plain or host:) has no destination.
func requireDownloadSource(src, dst string) error {
	if dst != "" {
		return nil
	}
	switch fleetclient.ParseCopyEndpoint(src).Kind {
	case fleetclient.CopyLocal, fleetclient.CopyHost:
		return fmt.Errorf("destination required: %q is on your machine", src)
	}
	return nil
}

// hostCopy resolves both endpoints against the host fleet and runs the generic
// copy engine over the dialled fleet server, with the CLI's own disk as local.
func hostCopy(ctx context.Context, src, dst string) (fleetclient.CopyResult, error) {
	srcEnd, err := resolveHostEndpoint(src)
	if err != nil {
		return fleetclient.CopyResult{}, err
	}
	dstEnd, err := resolveHostEndpoint(dst)
	if err != nil {
		return fleetclient.CopyResult{}, err
	}
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return fleetclient.CopyResult{}, err
	}
	defer conn.Close()
	return fleetclient.Copy(ctx, conn.Service(), srcEnd, dstEnd, fleetclient.HostLocalPolicy{})
}

// resolveHostEndpoint turns a typed argument into a ResolvedEndpoint for the host
// form: a plain or `host:` path is the host's own disk, an instance reference
// resolves its fleet (a bare instance infers it from the cwd, like the rest of
// the CLI), and `:path` (self) is rejected — there is no current instance on a host.
func resolveHostEndpoint(arg string) (fleetclient.ResolvedEndpoint, error) {
	ep := fleetclient.ParseCopyEndpoint(arg)
	switch ep.Kind {
	case fleetclient.CopyLocal, fleetclient.CopyHost:
		return fleetclient.ResolvedEndpoint{Local: true, Path: ep.Path}, nil
	case fleetclient.CopySelf:
		return fleetclient.ResolvedEndpoint{}, fmt.Errorf("`:path` only works inside an instance (got %q)", arg)
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
