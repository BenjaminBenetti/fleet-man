package fleet

import (
	"fmt"
	"regexp"
	"strings"
)

// Automation (issue #188) lets a fleet define Triggers that spin up Agents.
// Both lists are per-fleet config persisted inline on FleetSettings, validated
// here (TUI validates for immediate UX feedback; the server validates
// authoritatively in SetFleetSettings) and surfaced in the TUI's automation
// view. An Agent describes HOW a worker is launched (command + env); a Trigger
// describes WHEN, and names the Agent(s) it activates with the prompt to feed
// them.

// DefaultAgentCommand is the initial command offered for a new automation
// agent. ${SYS_PROMPT} and ${PROMPT} are substituted at trigger time; the user
// need not use either placeholder. The trailing spaces leave the cursor past
// the system-prompt flag so a user who prefers to append the prompt inline can
// just keep typing.
const DefaultAgentCommand = "claude --system-prompt '${SYS_PROMPT}'  "

// maxAutomationListLen bounds a fleet's trigger / agent lists so a corrupt or
// hostile settings write cannot mint an absurd number of entries (mirrors the
// bound on layout presets).
const maxAutomationListLen = 128

// TriggerType is the broad category of an automation trigger.
type TriggerType string

const (
	// TriggerSchedule fires its agents on a cron schedule.
	TriggerSchedule TriggerType = "schedule"
	// TriggerWebhook fires its agents when a matching event arrives on the
	// fleet gateway's webhook endpoint. (Delivering those events is out of
	// scope for issue #188 — only the definition is modeled here.)
	TriggerWebhook TriggerType = "webhook"
)

// WebhookFilterType selects how a webhook trigger decides whether an incoming
// event should fire its agents.
type WebhookFilterType string

const (
	// WebhookFilterRegex matches the raw event body against a regular
	// expression.
	WebhookFilterRegex WebhookFilterType = "regex"
	// WebhookFilterJSONPath compares the value at a JSON path against an
	// expected value.
	WebhookFilterJSONPath WebhookFilterType = "jsonpath"
)

// Agent is an automation worker definition: the command to launch and the
// environment to launch it in. Agents are spun up as ordinary fleet instances
// (they appear in the fleet view like any other instance) — they are just
// instances that a trigger created.
type Agent struct {
	// Name is the user-chosen label, unique within a fleet's agent list and
	// referenced by triggers.
	Name string `json:"name"`

	// Command is the shell command that starts the agent. It may contain the
	// ${PROMPT} and ${SYS_PROMPT} placeholders, substituted at trigger time
	// with the trigger's prompt and this agent's SystemPrompt respectively.
	// Empty falls back to DefaultAgentCommand at normalization time.
	Command string `json:"command,omitempty"`

	// TmuxMode runs Command in a fresh tmux session and sends the prompt via
	// send-keys, so the user can open the session in the TUI and watch the
	// agent work. Default ON (the new-agent dialog sets it). With TmuxMode on,
	// the agent's instance is torn down after the agent goes inactive for
	// longer than the automation idle timeout.
	TmuxMode bool `json:"tmuxMode"`

	// SystemPrompt steers the agent; it is injected into the ${SYS_PROMPT}
	// placeholder of Command.
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Backend is the env backend the agent's instance is provisioned on, the
	// same set offered when creating an instance. Empty falls back to
	// BackendDevcontainer.
	Backend BackendType `json:"backend,omitempty"`
}

// Trigger is an automation trigger: it activates one or more Agents with a
// prompt when its condition (a schedule or a webhook event) is met.
type Trigger struct {
	// Name is the user-chosen label, unique within a fleet's trigger list.
	Name string `json:"name"`

	// Type is the trigger category (schedule or webhook). Fields below are
	// interpreted per type (struct composition via a flat record).
	Type TriggerType `json:"type"`

	// AgentNames are the agents this trigger activates (each must name an Agent
	// in the same fleet). At least one is required.
	AgentNames []string `json:"agentNames,omitempty"`

	// Prompt is fed to the activated agents (into ${PROMPT}).
	Prompt string `json:"prompt,omitempty"`

	// Cron is the schedule expression (TriggerSchedule only): a standard 5-field
	// cron pattern.
	Cron string `json:"cron,omitempty"`

	// WebhookName is appended to the fleet gateway's webhook URL for this
	// trigger (TriggerWebhook only).
	WebhookName string `json:"webhookName,omitempty"`

	// FilterType selects regex vs json-path filtering (TriggerWebhook only).
	FilterType WebhookFilterType `json:"filterType,omitempty"`

	// Regex filters incoming events when FilterType is regex: the event fires
	// the agents when the expression matches.
	Regex string `json:"regex,omitempty"`

	// JSONPath selects the value compared against JSONValue when FilterType is
	// json-path.
	JSONPath string `json:"jsonPath,omitempty"`

	// JSONValue is the value the JSONPath selection must equal to fire.
	JSONValue string `json:"jsonValue,omitempty"`
}

// TmuxEnabled reports whether the agent runs in a tmux session. (Plain accessor
// kept for symmetry with the rest of the model and to centralize the default.)
func (a Agent) TmuxEnabled() bool { return a.TmuxMode }

// NormalizeAgent validates a single agent and returns its canonical form: the
// name is trimmed and must be non-empty, an empty command falls back to the
// default, and the backend defaults to devcontainer and must be valid.
func NormalizeAgent(a Agent) (Agent, error) {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return Agent{}, fmt.Errorf("agent name is empty")
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" {
		a.Command = strings.TrimSpace(DefaultAgentCommand)
	}
	if a.Backend == "" {
		a.Backend = BackendDevcontainer
	}
	if err := ValidateBackendType(a.Backend); err != nil {
		return Agent{}, fmt.Errorf("agent %q: %w", a.Name, err)
	}
	return a, nil
}

// NormalizeAgents validates every agent and returns the normalized list (input
// order preserved). A duplicate name (after trimming) is rejected — like layout
// presets, two agents sharing a name are ambiguous, not redundant no-ops.
func NormalizeAgents(in []Agent) ([]Agent, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxAutomationListLen {
		return nil, fmt.Errorf("too many agents (%d, max %d)", len(in), maxAutomationListLen)
	}
	out := make([]Agent, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		norm, err := NormalizeAgent(raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[norm.Name]; dup {
			return nil, fmt.Errorf("duplicate agent name %q", norm.Name)
		}
		seen[norm.Name] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}

// NormalizeTrigger validates a single trigger against the set of known agent
// names and returns its canonical form. Fields not relevant to the trigger's
// type are cleared so the persisted record stays clean (e.g. a schedule trigger
// never carries stale webhook fields).
func NormalizeTrigger(t Trigger, agentNames map[string]struct{}) (Trigger, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return Trigger{}, fmt.Errorf("trigger name is empty")
	}

	// Validate + dedup the agent references.
	if len(t.AgentNames) == 0 {
		return Trigger{}, fmt.Errorf("trigger %q activates no agents", t.Name)
	}
	seenAgent := make(map[string]struct{}, len(t.AgentNames))
	agents := make([]string, 0, len(t.AgentNames))
	for _, name := range t.AgentNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seenAgent[name]; dup {
			continue
		}
		if _, ok := agentNames[name]; !ok {
			return Trigger{}, fmt.Errorf("trigger %q references unknown agent %q", t.Name, name)
		}
		seenAgent[name] = struct{}{}
		agents = append(agents, name)
	}
	if len(agents) == 0 {
		return Trigger{}, fmt.Errorf("trigger %q activates no agents", t.Name)
	}
	t.AgentNames = agents

	switch t.Type {
	case TriggerSchedule:
		t.Cron = strings.TrimSpace(t.Cron)
		if err := ValidateCron(t.Cron); err != nil {
			return Trigger{}, fmt.Errorf("trigger %q: %w", t.Name, err)
		}
		// Clear webhook-only fields.
		t.WebhookName, t.FilterType, t.Regex, t.JSONPath, t.JSONValue = "", "", "", "", ""
	case TriggerWebhook:
		t.WebhookName = strings.TrimSpace(t.WebhookName)
		if t.WebhookName == "" {
			return Trigger{}, fmt.Errorf("trigger %q: webhook name is empty", t.Name)
		}
		switch t.FilterType {
		case WebhookFilterRegex:
			t.Regex = strings.TrimSpace(t.Regex)
			if t.Regex == "" {
				return Trigger{}, fmt.Errorf("trigger %q: webhook regex is empty", t.Name)
			}
			if _, err := regexp.Compile(t.Regex); err != nil {
				return Trigger{}, fmt.Errorf("trigger %q: invalid webhook regex: %w", t.Name, err)
			}
			t.JSONPath, t.JSONValue = "", ""
		case WebhookFilterJSONPath:
			t.JSONPath = strings.TrimSpace(t.JSONPath)
			if t.JSONPath == "" {
				return Trigger{}, fmt.Errorf("trigger %q: webhook json path is empty", t.Name)
			}
			t.JSONValue = strings.TrimSpace(t.JSONValue)
			t.Regex = ""
		default:
			return Trigger{}, fmt.Errorf("trigger %q: invalid webhook filter type %q", t.Name, t.FilterType)
		}
		// Clear schedule-only fields.
		t.Cron = ""
	default:
		return Trigger{}, fmt.Errorf("trigger %q: invalid type %q", t.Name, t.Type)
	}
	return t, nil
}

// NormalizeTriggers validates every trigger against the agents it may reference
// and returns the normalized list (input order preserved). Duplicate trigger
// names are rejected. agents is the fleet's (already-normalized) agent list.
func NormalizeTriggers(in []Trigger, agents []Agent) ([]Trigger, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxAutomationListLen {
		return nil, fmt.Errorf("too many triggers (%d, max %d)", len(in), maxAutomationListLen)
	}
	agentNames := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		agentNames[a.Name] = struct{}{}
	}
	out := make([]Trigger, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		norm, err := NormalizeTrigger(raw, agentNames)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[norm.Name]; dup {
			return nil, fmt.Errorf("duplicate trigger name %q", norm.Name)
		}
		seen[norm.Name] = struct{}{}
		out = append(out, norm)
	}
	return out, nil
}

// SubstituteAgentCommand expands the ${PROMPT} and ${SYS_PROMPT} placeholders in
// an agent command. ${SYS_PROMPT} is expanded first and ${PROMPT} second; a
// ${PROMPT} appearing inside the system prompt text is therefore NOT re-expanded
// (the system prompt is substituted before the prompt pass runs, but the prompt
// pass only scans the original command's ${PROMPT} occurrences... ) — to keep
// that guarantee we expand in a single pass via a replacer.
func SubstituteAgentCommand(command, prompt, systemPrompt string) string {
	r := strings.NewReplacer(
		"${SYS_PROMPT}", systemPrompt,
		"${PROMPT}", prompt,
	)
	return r.Replace(command)
}
