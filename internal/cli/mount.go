package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// mount.go is the `fleet mount` command tree: a fleet's custom mounts and the
// daemon-host folder each may use instead of its .mnt folder (the same
// settings the TUI's fleet options edit). Like automation, every write is a
// read-modify-write of FleetSettings, the shared fleet.SetCustomMount /
// RemoveCustomMount owning the rules; the other fields ride along.

// loadMounts fetches the named fleet's custom mounts (a seam for tests).
var loadMounts = func(ctx context.Context, fleetName string) (fleet.FleetSettings, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	defer conn.Close()
	reply, err := conn.Service().GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fleet.FleetSettings{}, fmt.Errorf("fleet %q not found", fleetName)
	}
	ps := pf.GetSettings()
	return fleet.FleetSettings{CustomMounts: ps.GetCustomMounts(), CustomMountHostFolders: ps.GetCustomMountHostFolders()}, nil
}

// mutateMounts applies fn to the named fleet's custom mounts and writes the
// settings back, every other field as it was; it returns the daemon's
// warnings (a host folder missing on the daemon host). A seam for tests.
var mutateMounts = func(ctx context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) ([]string, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	svc := conn.Service()
	reply, err := svc.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return nil, err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return nil, fmt.Errorf("fleet %q not found", fleetName)
	}
	ps := pf.GetSettings()
	if ps == nil {
		ps = &fleetgrpc.FleetSettings{}
	}
	settings, err := fn(fleet.FleetSettings{CustomMounts: ps.GetCustomMounts(), CustomMountHostFolders: ps.GetCustomMountHostFolders()})
	if err != nil {
		return nil, err
	}
	ps.CustomMounts, ps.CustomMountHostFolders = settings.CustomMounts, settings.CustomMountHostFolders
	saved, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{Fleet: fleetName, Settings: ps})
	if err != nil {
		return nil, err
	}
	return saved.GetWarnings(), nil
}

// newMountCmd builds the `fleet mount` command group.
func newMountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mount",
		Short: "Manage a fleet's custom mounts",
	}
	cmd.AddCommand(newMountListCmd(), newMountAddCmd(), newMountRemoveCmd())
	return cmd
}

func newMountListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list <fleet>",
		Aliases: []string{"ls"},
		Short:   "List a fleet's custom mounts",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadMounts(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PATH\tHOST")
			for _, p := range s.CustomMounts {
				host := s.CustomMountHostFolders[p]
				if host == "" {
					host = "-"
				}
				fmt.Fprintf(w, "%s\t%s\n", p, host)
			}
			return w.Flush()
		},
	}
}

func newMountAddCmd() *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "add <fleet> <path>",
		Short: "Add a custom mount, or change its host folder",
		Long: `Add a custom mount at <path>, or re-point one the fleet has. --host binds
a daemon-host folder as is (synced both ways on a cluster) instead of the
fleet's .mnt folder; without it the mount goes back there.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, path := args[0], args[1]
			warnings, err := mutateMounts(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.SetCustomMount(s, path, host)
			})
			if err != nil {
				return err
			}
			norm, _ := fleet.NormalizeCustomMount(path)
			fmt.Fprintf(cmd.OutOrStdout(), "Mounted %s in fleet %q\n", norm, fleetName)
			printWarnings(cmd.ErrOrStderr(), warnings)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "folder on the daemon host to bind (default: the fleet's .mnt folder)")
	return cmd
}

func newMountRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <fleet> <path>",
		Aliases: []string{"rm"},
		Short:   "Remove a custom mount",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, path := args[0], args[1]
			warnings, err := mutateMounts(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.RemoveCustomMount(s, path)
			})
			if err != nil {
				return err
			}
			norm, _ := fleet.NormalizeCustomMount(path)
			fmt.Fprintf(cmd.OutOrStdout(), "Removed mount %s from fleet %q\n", norm, fleetName)
			printWarnings(cmd.ErrOrStderr(), warnings)
			return nil
		},
	}
}
