package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/spf13/cobra"
)

// newPortForwardCmd creates the port-forward command group.
func newPortForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "port-forward",
		Aliases: []string{"pf"},
		Short:   "Manage port forwards for instances",
	}

	cmd.AddCommand(
		newPortForwardAddCmd(),
		newPortForwardRemoveCmd(),
		newPortForwardListCmd(),
	)

	return cmd
}

// parsePortMappingArg splits a "local:remote" string into two port numbers.
func parsePortMappingArg(mapping string) (int, int, error) {
	parts := strings.SplitN(mapping, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid port mapping %q: expected local:remote (e.g. 8080:80)", mapping)
	}
	local, err := strconv.Atoi(parts[0])
	if err != nil || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("invalid local port %q", parts[0])
	}
	remote, err := strconv.Atoi(parts[1])
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, fmt.Errorf("invalid remote port %q", parts[1])
	}
	return local, remote, nil
}

// newPortForwardAddCmd creates the `port-forward add` command.
func newPortForwardAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <instance> <local:remote> [local:remote...]",
		Short: "Forward local ports to an instance",
		Long:  "Forward one or more local ports to ports inside an instance.\nRuns until interrupted with Ctrl+C.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, instance, err := resolveInstance(args[0], "")
			if err != nil {
				return err
			}
			if instance.Status != fleet.StatusRunning {
				return fmt.Errorf("instance %s/%s is not running (status: %s)", target.Fleet, target.Instance, instance.Status)
			}

			// One server connection for the lifetime of the command: every
			// accepted local connection is tunnelled to the instance over a
			// Forward bidi stream (the data plane), so the forward works even
			// against a remote server.
			conn, err := fleetclient.Dial(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()

			manager := portforward.NewManager()
			key := target.Fleet + "/" + target.Instance

			for _, mapping := range args[1:] {
				local, remote, err := parsePortMappingArg(mapping)
				if err != nil {
					manager.Shutdown()
					return err
				}
				dial := portforward.NewGRPCTarget(conn.Service(), target.Fleet, target.Instance, remote)
				if err := manager.Add(key, local, remote, dial); err != nil {
					manager.Shutdown()
					return err
				}
				fmt.Printf("Forwarding localhost:%d -> %s:%d\n", local, instance.Name, remote)
			}

			fmt.Println("Press Ctrl+C to stop all forwards.")

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			<-sigChan

			fmt.Println("\nStopping port forwards...")
			manager.Shutdown()
			return nil
		},
	}
}

// newPortForwardRemoveCmd creates the `port-forward remove` command.
func newPortForwardRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <instance> <local-port>",
		Aliases: []string{"rm"},
		Short:   "Remove a port forward",
		Long:    "Note: CLI-started forwards run in the foreground and are stopped with Ctrl+C.\nThis command is primarily for scripting and will list active TUI forwards.",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %q", args[1])
			}

			fmt.Printf("Note: port-forward remove only works for forwards managed by the same process.\n")
			fmt.Printf("Use the TUI (press 'p') to manage forwards interactively, or Ctrl+C the 'port-forward add' process.\n")
			_ = target
			_ = port
			return nil
		},
	}
}

// newPortForwardListCmd creates the `port-forward list` command.
func newPortForwardListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list <instance>",
		Aliases: []string{"ls"},
		Short:   "List port forwards on an instance",
		Long:    "Shows any port forwards that are currently active.\nNote: Only forwards managed by the current TUI process are visible.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := fleet.Resolve(args[0], "")
			if err != nil {
				return err
			}

			fmt.Printf("Port forwards for %s/%s:\n", target.Fleet, target.Instance)
			fmt.Println("  (Use the TUI to manage port forwards interactively, or use 'fleet port-forward add' for CLI forwarding.)")
			return nil
		},
	}
}
