package cli

import (
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
	"github.com/BenjaminBenetti/fleet-man/internal/output"
	"github.com/spf13/cobra"
)

func newOutputCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "output", Short: "Play instance audio on a connected client"}
	cmd.AddCommand(&cobra.Command{Use: "devices", Short: "List this client's audio outputs", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		list, err := output.Devices()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "(default)\tSystem default")
		for _, d := range list {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", d.ID, d.Label)
		}
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "targets", Short: "List connected clients and their audio outputs", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := fleetclient.Dial(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		r, err := conn.Service().ListOutputTargets(cmd.Context(), &fleetgrpc.ListOutputTargetsRequest{})
		if err != nil {
			return err
		}
		for _, target := range r.GetTargets().GetTargets() {
			mark := " "
			if target.GetSelected() {
				mark = "*"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\t(default)\tSystem default\n", mark, target.GetClient())
			for _, d := range target.GetDevices() {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\t%s\n", target.GetClient(), d.GetId(), d.GetLabel())
			}
		}
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "attach", Short: "Offer this client's outputs until interrupted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := fleetclient.Dial(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		output.Run(cmd.Context(), conn.Service(), func(s output.Status) { fmt.Fprintln(cmd.ErrOrStderr(), s.Detail) })
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "source", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		return micsink.RunOutput(cmd.Context(), os.Stdin, os.Stdout)
	}})
	cmd.AddCommand(&cobra.Command{Use: "ensure", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true, RunE: func(*cobra.Command, []string) error { return micsink.EnsureOutput() }})
	cmd.AddCommand(&cobra.Command{Use: "stop", Hidden: true, Args: cobra.NoArgs, SilenceUsage: true, RunE: func(*cobra.Command, []string) error { return micsink.StopOutput() }})
	return cmd
}
