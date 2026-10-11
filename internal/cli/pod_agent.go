package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/podagent"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/spf13/cobra"
)

// newPodAgentCmd creates the hidden in-pod `fleet pod-agent`: the agent
// fleetd runs in every Kubernetes pod over a long-lived exec (see
// internal/podagent).
func newPodAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "pod-agent",
		Short:  "Fleet's agent inside a Kubernetes pod (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(newPodAgentServeCmd(), newPodAgentExecCmd(), newPodAgentSelfVersionCmd(), newPodAgentShepherdCmd(), newPodAgentSpokeCmd(), newPodAgentInstallBundleCmd())
	return cmd
}

func newPodAgentServeCmd() *cobra.Command {
	var proto int
	var role string
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Serve an agent session on stdio",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := podagent.DefaultOptions()
			opts.Role = podwire.Role(role)
			opts.DaemonProto = proto
			conn, err := podagent.TakeStdio()
			if err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-agent serve: %v\n", err)
				return err
			}
			if err := podagent.Serve(cmd.Context(), conn, opts); err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-agent serve: %v\n", err)
				return err
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&proto, "proto", 0, "fleetd's highest protocol version")
	cmd.Flags().StringVar(&role, "role", string(podwire.RoleMain), "session role: main or bulk")
	return cmd
}

func newPodAgentExecCmd() *cobra.Command {
	var stdio bool
	var resume string
	cmd := &cobra.Command{
		Use:           "exec --stdio",
		Short:         "Run one framed command over stdio",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !stdio {
				return errors.New("fleet pod-agent exec: --stdio is required")
			}
			if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				err := errors.New("fleet pod-agent exec reads frames on stdin, which must not be a terminal")
				fmt.Fprintln(os.Stderr, err)
				return err
			}
			if err := podagent.ExecStdio(cmd.Context(), os.Stdin, os.Stdout, podagent.DefaultOptions(), resume); err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-agent exec: %v\n", err)
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "frames on stdin and stdout")
	cmd.Flags().StringVar(&resume, "resume", "", "re-attach the held command with this resume id")
	return cmd
}

func newPodAgentSelfVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "self-version",
		Short: "Print this binary's version, sha256 and protocol range as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(podagent.SelfVersion())
		},
	}
}

func newPodAgentShepherdCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "shepherd",
		Short:         "Hold one command for the pod agent (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			return podagent.RunShepherd()
		},
	}
}

func newPodAgentSpokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "spoke",
		Short:         "Keep the fleet's share in sync for the pod agent (internal)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			return podagent.RunSpoke()
		},
	}
}

// exitBundleMissing is `install-bundle --check`'s exit code for a bundle
// that is not the installed one.
const exitBundleMissing = 3

func newPodAgentInstallBundleCmd() *cobra.Command {
	var opts podagent.BundleOptions
	var check bool
	cmd := &cobra.Command{
		Use:           "install-bundle --dir DIR --name NAME --sha256 SHA",
		Short:         "Install the gzip tar on stdin as DIR/NAME, content-addressed",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if check {
				if !podagent.BundleInstalled(opts) {
					exitPodAgent(exitBundleMissing)
				}
				return nil
			}
			if err := podagent.InstallBundle(cmd.InOrStdin(), opts); err != nil {
				fmt.Fprintf(os.Stderr, "fleet pod-agent install-bundle: %v\n", err)
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Dir, "dir", "/opt/fleet", "directory to install into")
	cmd.Flags().StringVar(&opts.Name, "name", "", "the bundle's link name")
	cmd.Flags().StringVar(&opts.SHA256, "sha256", "", "the archive's sha256")
	cmd.Flags().BoolVar(&check, "check", false, "exit 0 if installed, 3 if not; read nothing")
	return cmd
}

// exitPodAgent is os.Exit; a var so tests can catch it.
var exitPodAgent = os.Exit
