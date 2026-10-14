package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// sync.go is `fleet sync`: the shared folders (agent dirs, shared files,
// custom mounts) of fleets with instances on a cluster, which fleetd keeps
// converged between the daemon host and each cluster's share. Each
// subcommand is one RPC, served alike locally, through the gateway or over
// Armada SSH.

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Show or wait on the shared-folder sync with clusters",
		Long: "Show or wait on the sync of a fleet's shared folders between the daemon\n" +
			"host (~/.fleet/workspaces/<fleet>) and its clusters.",
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.SilenceUsage = true },
	}
	cmd.AddCommand(newSyncStatusCmd(), newSyncWaitCmd(), newSyncConflictsCmd())
	return cmd
}

func newSyncStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [<fleet>]",
		Short: "Show each fleet's sync: folders, what is pending, shares",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				reply, err := svc.GetShareSync(cmd.Context(), &fleetgrpc.GetShareSyncRequest{Fleet: firstArg(args)})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if asJSON {
					data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(reply)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(out, string(data))
					return err
				}
				if len(reply.GetFleets()) == 0 {
					if len(args) == 1 {
						fmt.Fprintf(out, "%s: nothing to sync (no instance on a cluster)\n", args[0])
					} else {
						fmt.Fprintln(out, "Nothing to sync: no fleet has an instance on a cluster.")
					}
					return nil
				}
				for i, f := range reply.GetFleets() {
					if i > 0 {
						fmt.Fprintln(out)
					}
					printShareSync(out, f)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the reply as JSON")
	return cmd
}

func newSyncWaitCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait [<fleet>]",
		Short: "Wait until a fleet's shared folders agree on the host and its clusters",
		Long: "Wait until a fleet's shared folders (every fleet's without one) agree on\n" +
			"the daemon host and in its clusters' shares. Exits 1, naming what is\n" +
			"behind, when they do not within --timeout.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				reply, err := svc.WaitShareSync(cmd.Context(), &fleetgrpc.WaitShareSyncRequest{Fleet: firstArg(args), TimeoutMs: timeout.Milliseconds()})
				if err != nil {
					return err
				}
				if reply.GetSynced() {
					return nil
				}
				for _, line := range reply.GetPending() {
					fmt.Fprintln(cmd.ErrOrStderr(), "  "+line)
				}
				return fmt.Errorf("not in sync within %s", timeout)
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to wait")
	return cmd
}

func newSyncConflictsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts [<fleet>]",
		Short: "List the conflict copies the sync kept on the daemon host",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				reply, err := svc.ListShareConflicts(cmd.Context(), &fleetgrpc.ListShareConflictsRequest{Fleet: firstArg(args)})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(reply.GetConflicts()) == 0 {
					fmt.Fprintln(out, "No conflict copies.")
					return nil
				}
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "TIME\tFLEET\tPATH\tSIDE\tREASON\tCOPY")
				for _, c := range reply.GetConflicts() {
					when := "-"
					if t := c.GetTime(); t != nil {
						when = t.AsTime().Local().Format("2006-01-02 15:04:05")
					}
					p := c.GetRoot() + "/" + c.GetPath()
					if loc := c.GetLocation(); loc != "" {
						p += " (" + loc + ")"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", when, c.GetFleet(), p, c.GetSide(), c.GetReason(), c.GetFile())
				}
				return tw.Flush()
			})
		},
	}
	return cmd
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// printShareSync prints one fleet's sync: its state line, its shares, then
// a row per folder.
func printShareSync(out io.Writer, f *fleetgrpc.FleetShareSync) {
	state := "in sync"
	switch {
	case len(f.GetPaused()) > 0:
		state = "sync paused: " + strings.Join(f.GetPaused(), "; ")
	case syncPending(f) > 0:
		state = "syncing"
	}
	fmt.Fprintf(out, "%s: %s\n", f.GetFleet(), state)
	for _, u := range f.GetShares() {
		line := fmt.Sprintf("  share %s on %s", u.GetClaim(), u.GetLocation())
		if u.GetClaim() == "" {
			line = "  share on " + u.GetLocation()
		}
		switch {
		case u.GetMeasured() && u.GetCapacityBytes() > 0:
			line += fmt.Sprintf(": %s of %s (%d%%)", syncBytes(u.GetUsedBytes()), syncBytes(u.GetCapacityBytes()), u.GetUsedBytes()*100/u.GetCapacityBytes())
		case u.GetCapacityBytes() > 0:
			line += ": " + syncBytes(u.GetCapacityBytes())
		}
		if u.GetExpandable() {
			line += ", grows when full"
		}
		if w := u.GetWarning(); w != "" {
			line += "; " + w
		}
		fmt.Fprintln(out, line)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  FOLDER\tFILES\tSIZE\tCAP\tSTATE")
	for _, r := range f.GetRoots() {
		limit := "-"
		if r.GetCap() > 0 {
			limit = syncBytes(r.GetCap())
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\n", r.GetRoot(), r.GetFiles(), syncBytes(r.GetBytes()), limit, rootState(r))
	}
	_ = tw.Flush()
	for _, r := range f.GetRoots() {
		for _, w := range r.GetWarnings() {
			fmt.Fprintln(out, "  warning: "+w)
		}
		for _, side := range r.GetSides() {
			for _, why := range side.GetRefused() {
				fmt.Fprintf(out, "  %s refused %s/%s\n", side.GetSide(), r.GetRoot(), why)
			}
		}
	}
	if n := f.GetConflicts(); n > 0 {
		copies := "copies"
		if n == 1 {
			copies = "copy"
		}
		fmt.Fprintf(out, "  %d conflict %s kept (fleet sync conflicts %s)\n", n, copies, f.GetFleet())
	}
}

// rootState is a folder's state: in sync, what is still to send each side,
// or why it is paused.
func rootState(r *fleetgrpc.ShareSyncRoot) string {
	if b := r.GetBroken(); b != "" {
		return "not synced: " + b
	}
	var parts []string
	if p := r.GetPaused(); p != "" {
		parts = append(parts, "paused: "+p)
	}
	for _, side := range r.GetSides() {
		switch {
		case side.GetPaused() != "":
			parts = append(parts, fmt.Sprintf("%s paused: %s (%d to send)", side.GetSide(), side.GetPaused(), side.GetPending()))
		case side.GetPending() > 0:
			parts = append(parts, fmt.Sprintf("%d to send to %s", side.GetPending(), side.GetSide()))
		}
		if e := side.GetLastError(); e != "" && side.GetPending() > 0 {
			parts[len(parts)-1] += " (" + e + ")"
		}
	}
	if len(parts) == 0 {
		return "in sync"
	}
	return strings.Join(parts, "; ")
}

func syncPending(f *fleetgrpc.FleetShareSync) int {
	n := 0
	for _, r := range f.GetRoots() {
		for _, side := range r.GetSides() {
			n += int(side.GetPending())
		}
	}
	return n
}

// syncBytes prints a size for people.
func syncBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
