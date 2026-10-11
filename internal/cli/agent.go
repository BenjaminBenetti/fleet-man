package cli

import (
	"context"
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", a.Name, a.DeployTarget(), triggersUsing(settings.Triggers, a.Name), a.Command)
			}
			return w.Flush()
		},
	}
}

func newAgentCreateCmd() *cobra.Command {
	var command, systemPrompt, backend, cluster string
	cmd := &cobra.Command{
		Use:   "create <fleet> <name>",
		Short: "Create an automation agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			flags := cmd.Flags()
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				a := fleet.Agent{
					Name:         name,
					Command:      command,
					SystemPrompt: systemPrompt,
					Backend:      fleet.BackendType(backend),
				}
				if err := setAgentTarget(cmd.Context(), &a, backend, flags.Changed("backend"), cluster, flags.Changed("cluster")); err != nil {
					return s, err
				}
				return fleet.AddAgent(s, a)
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
	f.StringVar(&backend, "backend", string(fleet.BackendDevcontainer), "env backend: devcontainer, coder, codespaces or kubernetes")
	f.StringVar(&cluster, "cluster", "", "Kubernetes connection to run on (id, name or context; implies --backend kubernetes)")
	return cmd
}

func newAgentEditCmd() *cobra.Command {
	var command, systemPrompt, backend, cluster, rename string
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
				if flags.Changed("backend") || flags.Changed("cluster") {
					if err := setAgentTarget(cmd.Context(), &a, backend, flags.Changed("backend"), cluster, flags.Changed("cluster")); err != nil {
						return s, err
					}
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
	f.StringVar(&backend, "backend", "", "env backend: devcontainer, coder, codespaces or kubernetes")
	f.StringVar(&cluster, "cluster", "", "Kubernetes connection to run on (id, name or context; implies --backend kubernetes)")
	return cmd
}

// setAgentTarget points a at the target the --backend and --cluster flags
// name (each set says whether it was given): a cluster implies kubernetes and
// is stored as its connection id; kubernetes without --cluster keeps the
// agent's own connection, else takes the only one. Without either flag a
// new agent runs on devcontainer, never on fleetd's remembered target:
// agents fire unattended.
func setAgentTarget(ctx context.Context, a *fleet.Agent, backend string, backendSet bool, cluster string, clusterSet bool) error {
	if clusterSet {
		if backendSet && fleet.BackendType(backend) != fleet.BackendKubernetes {
			return fmt.Errorf("--cluster runs the agent's instances on Kubernetes, not %s", backend)
		}
		a.Backend = fleet.BackendKubernetes
	} else if backendSet {
		if fleet.BackendType(backend) != fleet.BackendKubernetes || a.Backend != fleet.BackendKubernetes {
			a.KubeCluster = ""
		}
		a.Backend = fleet.BackendType(backend)
	}
	if a.Backend != fleet.BackendKubernetes || (!clusterSet && a.KubeCluster != "") {
		return nil
	}
	id, err := agentClusterID(ctx, cluster)
	if err != nil {
		return err
	}
	a.KubeCluster = id
	return nil
}

// agentClusterID resolves a --cluster reference (an id, a name, or a context
// one connection uses; "" for the only connection) to its connection id on
// the daemon. A package var so the command tests can skip the daemon.
var agentClusterID = func(ctx context.Context, ref string) (string, error) {
	var id string
	err := withCluster(ctx, func(svc fleetgrpc.FleetServiceClient) error {
		reply, err := svc.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
		if err != nil {
			return err
		}
		c, err := fleetgrpc.FindOrOnlyKubeCluster(reply.GetClusters(), ref)
		if err != nil {
			return err
		}
		id = c.GetCluster().GetId()
		return nil
	})
	return id, err
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
