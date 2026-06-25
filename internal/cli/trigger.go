package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/spf13/cobra"
)

// trigger.go is the `fleet trigger` command tree (issue #189): CRUD over a
// fleet's automation triggers. A trigger fires one or more of the fleet's
// agents (with a prompt) when its condition is met — a cron schedule, a gateway
// webhook event, or a cron-scheduled bash command that exits zero. The shared
// read-modify-write plumbing lives in automation.go; the type-specific field
// validation lives in fleet.NormalizeTrigger (which AddTrigger/UpdateTrigger apply).

// newTriggerCmd builds the `fleet trigger` command group.
func newTriggerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger",
		Short: "Manage a fleet's automation triggers",
	}
	cmd.AddCommand(
		newTriggerListCmd(),
		newTriggerCreateCmd(),
		newTriggerEditCmd(),
		newTriggerDeleteCmd(),
		newTriggerLogsCmd(),
	)
	return cmd
}

func newTriggerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list <fleet>",
		Aliases: []string{"ls"},
		Short:   "List a fleet's automation triggers",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := loadAutomation(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tAGENTS\tDETAIL")
			for _, t := range settings.Triggers {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, t.Type, strings.Join(t.AgentNames, ","), triggerDetail(t))
			}
			return w.Flush()
		},
	}
}

// triggerFlags are the per-field flags shared by create and edit.
type triggerFlags struct {
	triggerType string
	agents      []string
	prompt      string
	cron        string
	script      string
	webhookName string
	filterType  string
	regex       string
	jsonPath    string
	jsonValue   string
}

func (tf *triggerFlags) bind(cmd *cobra.Command, typeDefault, filterDefault string) {
	f := cmd.Flags()
	f.StringVar(&tf.triggerType, "type", typeDefault, "trigger type: schedule, webhook, or bash")
	f.StringArrayVar(&tf.agents, "agent", nil, "agent to activate (repeatable; at least one required)")
	f.StringVar(&tf.prompt, "prompt", "", "prompt fed to the agents via ${PROMPT}")
	f.StringVar(&tf.cron, "cron", "", "5-field cron expression (schedule and bash types)")
	f.StringVar(&tf.script, "script", "", "bash command run when the cron is due; zero exit fires, stdout is the payload (bash type)")
	f.StringVar(&tf.webhookName, "webhook-name", "", "webhook: name appended to the gateway URL (webhook type)")
	f.StringVar(&tf.filterType, "filter-type", filterDefault, "webhook: filter type, regex or jsonpath (webhook type)")
	f.StringVar(&tf.regex, "regex", "", "webhook: regex the event body must match (regex filter)")
	f.StringVar(&tf.jsonPath, "json-path", "", "webhook: JSON path to compare (jsonpath filter)")
	f.StringVar(&tf.jsonValue, "json-value", "", "webhook: value the JSON path must equal (jsonpath filter)")
}

// apply writes the flag values onto t. When onlyChanged is true (edit), a field
// is written only if its flag was explicitly set, so unspecified fields keep
// their stored value.
func (tf *triggerFlags) apply(cmd *cobra.Command, t *fleet.Trigger, onlyChanged bool) {
	flags := cmd.Flags()
	set := func(name string, write func()) {
		if !onlyChanged || flags.Changed(name) {
			write()
		}
	}
	set("type", func() { t.Type = fleet.TriggerType(tf.triggerType) })
	set("agent", func() { t.AgentNames = tf.agents })
	set("prompt", func() { t.Prompt = tf.prompt })
	set("cron", func() { t.Cron = tf.cron })
	set("script", func() { t.Script = tf.script })
	set("webhook-name", func() { t.WebhookName = tf.webhookName })
	set("filter-type", func() { t.FilterType = fleet.WebhookFilterType(tf.filterType) })
	set("regex", func() { t.Regex = tf.regex })
	set("json-path", func() { t.JSONPath = tf.jsonPath })
	set("json-value", func() { t.JSONValue = tf.jsonValue })
}

func newTriggerCreateCmd() *cobra.Command {
	var tf triggerFlags
	cmd := &cobra.Command{
		Use:   "create <fleet> <name>",
		Short: "Create an automation trigger",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				t := fleet.Trigger{Name: name}
				tf.apply(cmd, &t, false)
				return fleet.AddTrigger(s, t)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created trigger %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
	tf.bind(cmd, string(fleet.TriggerSchedule), string(fleet.WebhookFilterRegex))
	return cmd
}

func newTriggerEditCmd() *cobra.Command {
	var tf triggerFlags
	var rename string
	cmd := &cobra.Command{
		Use:   "edit <fleet> <name>",
		Short: "Edit an automation trigger (only the flags you pass change)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				t, ok := fleet.FindTrigger(s.Triggers, name)
				if !ok {
					return s, fmt.Errorf("trigger %q not found", name)
				}
				if cmd.Flags().Changed("rename") {
					t.Name = rename
				}
				tf.apply(cmd, &t, true)
				return fleet.UpdateTrigger(s, name, t)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated trigger %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
	cmd.Flags().StringVar(&rename, "rename", "", "new name")
	tf.bind(cmd, string(fleet.TriggerSchedule), string(fleet.WebhookFilterRegex))
	return cmd
}

func newTriggerDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <fleet> <name>",
		Aliases: []string{"rm"},
		Short:   "Delete an automation trigger",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			err := mutateAutomation(cmd.Context(), fleetName, func(s fleet.FleetSettings) (fleet.FleetSettings, error) {
				return fleet.DeleteTrigger(s, name)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted trigger %q in fleet %q\n", name, fleetName)
			return nil
		},
	}
}

func newTriggerLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <fleet> <name>",
		Short: "Show a trigger's recorded event logs",
		Long: "Show a trigger's recorded event logs: the payloads of its recent firings\n" +
			"(webhook body, bash command stdout, or schedule fire-time). The last 100 firings are kept.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetName, name := args[0], args[1]
			logs, err := triggerLogs(cmd.Context(), fleetName, name)
			if err != nil {
				return err
			}
			if strings.TrimSpace(logs) == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "No events recorded for trigger %q in fleet %q\n", name, fleetName)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), logs)
			return nil
		},
	}
}

// triggerDetail is the one-line schedule/webhook/bash summary shown by `list`.
func triggerDetail(t fleet.Trigger) string {
	switch t.Type {
	case fleet.TriggerSchedule:
		return "cron: " + t.Cron
	case fleet.TriggerBash:
		return fmt.Sprintf("cron: %s sh: %s", t.Cron, firstLine(t.Script))
	case fleet.TriggerWebhook:
		switch t.FilterType {
		case fleet.WebhookFilterJSONPath:
			return fmt.Sprintf("webhook:%s jsonpath %s=%s", t.WebhookName, t.JSONPath, t.JSONValue)
		default:
			return fmt.Sprintf("webhook:%s regex %s", t.WebhookName, t.Regex)
		}
	default:
		return string(t.Type)
	}
}

// firstLine returns s's first line, truncated with an ellipsis, so a multi-line
// or long bash script stays on one tidy row of the `list` table. Truncation is
// rune-aware so it never splits a multibyte character.
func firstLine(s string) string {
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	const max = 40
	if r := []rune(line); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return line
}
