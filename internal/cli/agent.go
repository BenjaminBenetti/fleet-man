package cli

import (
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/BenjaminBenetti/fleet-man/internal/agentstrategy"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

// agent.go is the `fleet agent` command tree (issue #189): CRUD over a fleet's
// automation agents. An agent defines how an automation worker is launched (a
// command with ${PROMPT}/${SYS_PROMPT} placeholders, a system prompt, an env
// backend, and whether it gets the fleet MCP); triggers reference agents by
// name to fire them. The shared
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
			fmt.Fprintln(w, "NAME\tBACKEND\tTRIGGERS\tMCP\tCOMMAND")
			for _, a := range settings.Agents {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", a.Name, a.Backend, triggersUsing(settings.Triggers, a.Name), fleetMCPColumn(a), a.Command)
			}
			return w.Flush()
		},
	}
}

func newAgentCreateCmd() *cobra.Command {
	var command, systemPrompt, backend string
	var fleetMCP bool
	cmd := &cobra.Command{
		Use:   "create <fleet> <name>",
		Short: "Create an automation agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			a := fleet.Agent{
				Name:         name,
				Command:      command,
				SystemPrompt: systemPrompt,
				Backend:      fleet.BackendType(backend),
				FleetMCP:     fleetMCP,
			}
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.AddAgent(s, a)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created agent %q in fleet %q\n", name, fleetName)
			warnFleetMCP(cmd, a)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&command, "command", "", "launch command (${PROMPT}/${SYS_PROMPT} substituted; default: "+fleet.DefaultAgentCommand+")")
	f.StringVar(&systemPrompt, "system-prompt", "", "system prompt injected into ${SYS_PROMPT}")
	f.StringVar(&backend, "backend", string(fleet.BackendDevcontainer), "env backend: devcontainer, coder, or codespaces")
	f.BoolVar(&fleetMCP, "fleet-mcp", false, fleetMCPFlagHelp)
	return cmd
}

func newAgentEditCmd() *cobra.Command {
	var command, systemPrompt, backend, rename string
	var fleetMCP bool
	cmd := &cobra.Command{
		Use:   "edit <fleet> <name>",
		Short: "Edit an automation agent (only the flags you pass change)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			flags := cmd.Flags()
			var edited fleet.Agent
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
				if flags.Changed("fleet-mcp") {
					a.FleetMCP = fleetMCP
				}
				edited = a
				return fleet.UpdateAgent(s, name, a)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated agent %q in fleet %q\n", name, fleetName)
			warnFleetMCP(cmd, edited)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&rename, "rename", "", "new name (also rewrites triggers that reference this agent)")
	f.StringVar(&command, "command", "", "launch command (${PROMPT}/${SYS_PROMPT} substituted)")
	f.StringVar(&systemPrompt, "system-prompt", "", "system prompt injected into ${SYS_PROMPT}")
	f.StringVar(&backend, "backend", "", "env backend: devcontainer, coder, or codespaces")
	f.BoolVar(&fleetMCP, "fleet-mcp", false, fleetMCPFlagHelp+" (--fleet-mcp=false removes it)")
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

// fleetMCPFlagHelp describes --fleet-mcp.
const fleetMCPFlagHelp = "give the agent the full fleet MCP + fleet-admiral skill in its instance (Claude Code, devcontainer; allows host access from the instance)"

// warnFleetMCP warns on stderr when an agent has the fleet MCP on but could
// not use it, so the toggle does not silently do nothing.
func warnFleetMCP(cmd *cobra.Command, a fleet.Agent) {
	if !a.FleetMCP {
		return
	}
	if why := agentstrategy.FleetMCPUnsupported(a.Command, a.Backend); why != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: the fleet MCP %s; the agent will run without it\n", why)
	}
}

// fleetMCPColumn renders an agent's fleet MCP for the list: on, off, or on
// but unusable by this agent.
func fleetMCPColumn(a fleet.Agent) string {
	switch {
	case !a.FleetMCP:
		return "off"
	case agentstrategy.FleetMCPUnsupported(a.Command, a.Backend) != "":
		return "on (won't apply)"
	default:
		return "on"
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
