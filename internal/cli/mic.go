package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
	"github.com/spf13/cobra"
)

// newMicCmd creates the `fleet mic` command group: the virtual microphone.
//
// `devices` and `attach` are user-facing: the first lists what the Settings
// page's microphone selector offers on this machine, the second provides this
// machine's microphone without a TUI. `sink`, `ensure` and `stop` are hidden:
// fleet-man runs them INSIDE an instance, through its staged /usr/bin/fleet, and
// they are meaningless on the host.
func newMicCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mic",
		Short: "Virtual microphone",
		Long: `Proxy this machine's microphone into fleet instances, so voice input works
in a headless container. Enable it, and pick the device, under Settings ->
Microphone in the TUI. The microphone is only opened while something inside an
instance is recording.`,
	}
	cmd.AddCommand(newMicDevicesCmd(), newMicAttachCmd(), newMicSinkCmd(), newMicEnsureCmd(), newMicStopCmd())
	return cmd
}

func newMicDevicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List this machine's capture devices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			devices, err := mic.Devices()
			if err != nil {
				if mic.IsNoTool(err) {
					return errors.New(mic.Describe(err))
				}
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-40s %s\n", "(default)", mic.DefaultLabel)
			for _, device := range devices {
				fmt.Fprintf(out, "%-40s %s\n", device.ID, device.Label)
			}
			return nil
		},
	}
}

// newMicAttachCmd creates `fleet mic attach`: be the daemon's microphone
// provider from a plain terminal, exactly as an open TUI would. For driving
// fleet headlessly (or against a remote daemon) while still talking to agents.
func newMicAttachCmd() *cobra.Command {
	var device string
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Provide this machine's microphone until interrupted",
		Long: `Provide this machine's microphone to the fleet daemon until interrupted — what
an open TUI does on its own. The microphone must be enabled in Settings, and the
device chosen there is the one recorded (--device overrides it). It is only
opened while something inside an instance is recording; each transition is
printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := mic.Unavailable(); err != nil {
				return errors.New(mic.Describe(err))
			}
			conn, err := fleetclient.Dial(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()

			out := cmd.OutOrStdout()
			var final mic.Status
			var lastLine string
			// No --device: the daemon pushes the device chosen in Settings along
			// with every demand, exactly as it does for an open TUI.
			mic.Run(cmd.Context(), conn.Service(), device, func(status mic.Status) {
				final = status
				// One line per CHANGE: the provider re-reports on every retry.
				line := fmt.Sprintf("%d %v %s %v", status.State, status.Instances, status.Detail, status.FellBack)
				if line == lastLine {
					return
				}
				lastLine = line
				switch status.State {
				case mic.StateIdle:
					fmt.Fprintln(out, "idle")
				case mic.StateLive:
					line := "live -> " + strings.Join(status.Instances, ", ")
					if status.FellBack {
						line += " (configured device not found here; recording the system default)"
					}
					fmt.Fprintln(out, line)
				case mic.StateError:
					fmt.Fprintln(out, "error: "+status.Detail)
				case mic.StateDisabled:
					fmt.Fprintln(out, "disabled: enable the microphone in Settings")
				}
			})
			if final.State == mic.StateUnsupported {
				return fmt.Errorf("this fleet daemon predates the microphone; update it")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "capture device id from `fleet mic devices` (default: the device chosen in Settings)")
	return cmd
}

// newMicSinkCmd creates the hidden in-instance `fleet mic sink`: PCM on stdin
// becomes the instance's microphone; demand events go to stdout. The daemon
// holds one per running instance (see internal/server/mic.go).
func newMicSinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "sink",
		Short:         "Feed the instance's virtual microphone from stdin (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return micsink.Run(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
}

// newMicEnsureCmd creates the hidden in-instance `fleet mic ensure`: start the
// virtual-microphone server if it is not running. Provisioning calls it so a
// recorder probing for a microphone finds one before any client has attached.
func newMicEnsureCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "ensure",
		Short:        "Start the instance's virtual microphone server (internal)",
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return micsink.Ensure()
		},
	}
}

// newMicStopCmd creates the hidden in-instance `fleet mic stop`: shut the
// virtual-microphone server down. The daemon runs it when the feature is turned
// off, so the instance is left with no microphone rather than a silent one.
func newMicStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "stop",
		Short:        "Stop the instance's virtual microphone server (internal)",
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return micsink.Stop()
		},
	}
}
