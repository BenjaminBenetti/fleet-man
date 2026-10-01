package cli

import (
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

// agent.go is the `fleet agent` command tree (issue #189): CRUD over a fleet's
// automation agents. An agent defines how an automation worker is launched (a
// command with ${PROMPT}/${SYS_PROMPT} placeholders, a system prompt, and an
// env backend); triggers reference agents by name to fire them. The shared
// read-modify-write plumbing lives in automation.go.

// newAgentCmd builds the `fleet agent` command group.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage a fleet's automation agents",
	}
	cmd.AddCommand(
		newAgentListCmd(),
		newAgentCreateCmd(),
		newAgentEditCmd(),
		newAgentDeleteCmd(),
	)
	return cmd
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list <fleet>",
		Aliases: []string{"ls"},
		Short:   "List a fleet's automation agents",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := loadAutomation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tBACKEND\tTRIGGERS\tCOMMAND")
			for _, a := range settings.Agents {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", a.Name, a.Backend, triggersUsing(settings.Triggers, a.Name), a.Command)
			}
			return w.Flush()
		},
	}
}

func newAgentCreateCmd() *cobra.Command {
	var command, systemPrompt, backend string
	cmd := &cobra.Command{
		Use:   "create <fleet> <name>",
		Short: "Create an automation agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.AddAgent(s, fleet.Agent{
					Name:         name,
					Command:      command,
					SystemPrompt: systemPrompt,
					Backend:      fleet.BackendType(backend),
				})
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created agent %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&command, "command", "", "launch command (${PROMPT}/${SYS_PROMPT} substituted; default: "+fleet.DefaultAgentCommand+")")
	f.StringVar(&systemPrompt, "system-prompt", "", "system prompt injected into ${SYS_PROMPT}")
	f.StringVar(&backend, "backend", string(fleet.BackendDevcontainer), "env backend: devcontainer, coder, or codespaces")
	return cmd
}

func newAgentEditCmd() *cobra.Command {
	var command, systemPrompt, backend, rename string
	cmd := &cobra.Command{
		Use:   "edit <fleet> <name>",
		Short: "Edit an automation agent (only the flags you pass change)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			flags := cmd.Flags()
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				a, ok := fleet.FindAgent(s.Agents, name)
				if !ok {
					return s, fmt.Errorf("agent %q not found", name)
				}
				if flags.Changed("rename") {
					a.Name = rename
				}
				if flags.Changed("command") {
					a.Command = command
				}
				if flags.Changed("system-prompt") {
					a.SystemPrompt = systemPrompt
				}
				if flags.Changed("backend") {
					a.Backend = fleet.BackendType(backend)
				}
				return fleet.UpdateAgent(s, name, a)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated agent %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&rename, "rename", "", "new name (also rewrites triggers that reference this agent)")
	f.StringVar(&command, "command", "", "launch command (${PROMPT}/${SYS_PROMPT} substituted)")
	f.StringVar(&systemPrompt, "system-prompt", "", "system prompt injected into ${SYS_PROMPT}")
	f.StringVar(&backend, "backend", "", "env backend: devcontainer, coder, or codespaces")
	return cmd
}

func newAgentDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <fleet> <name>",
		Aliases: []string{"rm"},
		Short:   "Delete an automation agent (must not be referenced by a trigger)",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.DeleteAgent(s, name)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted agent %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
}

// triggersUsing counts the triggers that reference the named agent.
func triggersUsing(triggers []fleet.Trigger, agentName string) int {
	n := 0
	for _, t := range triggers {
		if slices.Contains(t.AgentNames, agentName) {
			n++
		}
	}
	return n
}
