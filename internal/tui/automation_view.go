package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// automation_view.go renders a fleet's in-page automation view (issue #188) and
// holds the shared helpers the trigger/agent dialogs build on: list accessors,
// the persist-to-server path, and item deletion. The two modal editors live in
// dialog_automation_trigger.go and dialog_automation_agent.go.

// newAutomationInput builds the single reusable text input the automation
// dialogs drive (one active field at a time: its value is loaded on edit and
// written back on commit, so one input covers every text field).
func newAutomationInput() textinput.Model {
	ti := textinput.New()
	ti.CharLimit = 1024
	ti.Width = 52
	return ti
}

// isAutomationRow reports whether a row kind belongs to the automation view.
func isAutomationRow(kind rowKind) bool {
	switch kind {
	case rowAutomationTriggers, rowAutomationAgents, rowTrigger, rowAgent, rowNewTrigger, rowNewAgent:
		return true
	}
	return false
}

// triggerAt / agentAt safely fetch the automation item a row points at.
func triggerAt(f *fleet.Fleet, idx int) *fleet.Trigger {
	if f == nil || idx < 0 || idx >= len(f.Settings.Triggers) {
		return nil
	}
	return &f.Settings.Triggers[idx]
}

func agentAt(f *fleet.Fleet, idx int) *fleet.Agent {
	if f == nil || idx < 0 || idx >= len(f.Settings.Agents) {
		return nil
	}
	return &f.Settings.Agents[idx]
}

// renderAutomationRow renders one automation-view row (group header, item, or
// "+ add" action). The caller handles trailing newline + width truncation.
func (fleetPage *fleetPage) renderAutomationRow(m *model, r row, cursor string, isSelected bool) string {
	f := m.st.Fleets[r.fleetName]
	switch r.kind {
	case rowAutomationTriggers, rowAutomationAgents:
		isTriggers := r.kind == rowAutomationTriggers
		label, n, collapsed := "agents", 0, fleetPage.agentsCollapsed(r.fleetName)
		if isTriggers {
			label, collapsed = "triggers", fleetPage.triggersCollapsed(r.fleetName)
			if f != nil {
				n = len(f.Settings.Triggers)
			}
		} else if f != nil {
			n = len(f.Settings.Agents)
		}
		arrow := "▼ "
		if collapsed {
			arrow = "▶ "
		}
		style := fleetExpandedStyle
		if isSelected {
			style = selectedStyle
		}
		return fmt.Sprintf("%s    %s%s", cursor, style.Render(arrow+label), dimStyle.Render(fmt.Sprintf(" (%d)", n)))

	case rowTrigger:
		t := triggerAt(f, r.autoIdx)
		if t == nil {
			return fmt.Sprintf("%s        %s", cursor, dimStyle.Render("(missing trigger)"))
		}
		style := sessionStyle
		if isSelected {
			style = selectedStyle
		} else if t.Disabled {
			style = dimStyle // a disabled trigger reads as inactive
		}
		return fmt.Sprintf("%s        %s%s", cursor, style.Render(t.Name), dimStyle.Render("  "+triggerSummary(*t)))

	case rowAgent:
		a := agentAt(f, r.autoIdx)
		if a == nil {
			return fmt.Sprintf("%s        %s", cursor, dimStyle.Render("(missing agent)"))
		}
		style := sessionStyle
		if isSelected {
			style = selectedStyle
		}
		return fmt.Sprintf("%s        %s%s", cursor, style.Render(a.Name), dimStyle.Render("  "+agentSummary(f, *a)))

	case rowNewTrigger:
		return renderAutomationAction(cursor, "+ add trigger", isSelected)
	case rowNewAgent:
		return renderAutomationAction(cursor, "+ add agent", isSelected)
	}
	return ""
}

func renderAutomationAction(cursor, label string, isSelected bool) string {
	style := newSessionStyle
	if isSelected {
		style = selectedStyle
	}
	return fmt.Sprintf("%s        %s", cursor, style.Render(label))
}

// triggerSummary is the dim one-line description shown beside a trigger's name.
func triggerSummary(t fleet.Trigger) string {
	var detail string
	switch t.Type {
	case fleet.TriggerSchedule:
		detail = "⏱ " + t.Cron
		if t.Cron == "" {
			detail = "⏱ (no schedule)"
		}
	case fleet.TriggerWebhook:
		detail = "⇄ webhook:" + t.WebhookName
	default:
		detail = string(t.Type)
	}
	if len(t.AgentNames) > 0 {
		detail += "  →  " + strings.Join(t.AgentNames, ", ")
	}
	if t.Disabled {
		detail += "  ·  disabled"
	}
	return detail
}

// agentSummary is the dim description shown beside an agent's name, including
// how many triggers reference it (the issue's "<- x" indicator).
func agentSummary(f *fleet.Fleet, a fleet.Agent) string {
	n := triggerCountForAgent(f, a.Name)
	parts := []string{backendTypeLabel(a.Backend)}
	suffix := "s"
	if n == 1 {
		suffix = ""
	}
	parts = append(parts, fmt.Sprintf("← %d trigger%s", n, suffix))
	return strings.Join(parts, " · ")
}

func triggerCountForAgent(f *fleet.Fleet, name string) int {
	if f == nil {
		return 0
	}
	n := 0
	for _, t := range f.Settings.Triggers {
		for _, an := range t.AgentNames {
			if an == name {
				n++
				break
			}
		}
	}
	return n
}

// persistAutomationSettings optimistically applies newSettings to the in-memory
// fleet and pushes them to the server, reverting on failure (the server is the
// trust boundary — it may reject a bad cron, duplicate name, etc.).
func (fleetPage *fleetPage) persistAutomationSettings(m *model, fleetName string, newSettings fleet.FleetSettings) error {
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return fmt.Errorf("fleet %s not found", fleetName)
	}
	prev := f.Settings
	f.Settings = newSettings
	if err := setFleetSettingsRemote(fleetName, newSettings); err != nil {
		f.Settings = prev
		return err
	}
	fleetPage.buildRows(m)
	return nil
}

// toggleTriggerDisabled flips a trigger's enabled state from the list (the 's'
// quick action) and persists. A defensive copy of the slice is mutated so the
// optimistic-revert in persistAutomationSettings can restore the original.
func (fleetPage *fleetPage) toggleTriggerDisabled(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok || idx < 0 || idx >= len(f.Settings.Triggers) {
		return nil
	}
	newSettings := f.Settings
	newTriggers := append([]fleet.Trigger(nil), f.Settings.Triggers...)
	newTriggers[idx].Disabled = !newTriggers[idx].Disabled
	newSettings.Triggers = newTriggers
	name, disabled := newTriggers[idx].Name, newTriggers[idx].Disabled
	if err := fleetPage.persistAutomationSettings(m, fleetName, newSettings); err != nil {
		m.message = fmt.Sprintf("Toggle failed: %v", err)
		return nil
	}
	if disabled {
		m.message = fmt.Sprintf("Disabled trigger %q", name)
	} else {
		m.message = fmt.Sprintf("Enabled trigger %q", name)
	}
	return nil
}

// deleteTrigger removes the trigger at idx and persists.
func (fleetPage *fleetPage) deleteTrigger(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok || idx < 0 || idx >= len(f.Settings.Triggers) {
		return nil
	}
	name := f.Settings.Triggers[idx].Name
	newSettings := f.Settings
	newSettings.Triggers = removeTriggerAt(f.Settings.Triggers, idx)
	if err := fleetPage.persistAutomationSettings(m, fleetName, newSettings); err != nil {
		m.message = fmt.Sprintf("Delete failed: %v", err)
		return nil
	}
	m.message = fmt.Sprintf("Deleted trigger %q", name)
	return nil
}

// deleteAgent removes the agent at idx and persists. An agent still referenced
// by a trigger is not deleted — the user is told to detach it first, so a delete
// can never orphan a trigger (which the server would then reject wholesale).
func (fleetPage *fleetPage) deleteAgent(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok || idx < 0 || idx >= len(f.Settings.Agents) {
		return nil
	}
	name := f.Settings.Agents[idx].Name
	for _, t := range f.Settings.Triggers {
		for _, an := range t.AgentNames {
			if an == name {
				m.message = fmt.Sprintf("Agent %q is used by trigger %q — remove it there first", name, t.Name)
				return nil
			}
		}
	}
	newSettings := f.Settings
	newSettings.Agents = removeAgentAt(f.Settings.Agents, idx)
	if err := fleetPage.persistAutomationSettings(m, fleetName, newSettings); err != nil {
		m.message = fmt.Sprintf("Delete failed: %v", err)
		return nil
	}
	m.message = fmt.Sprintf("Deleted agent %q", name)
	return nil
}

// removeTriggerAt / removeAgentAt return a NEW slice with the element at idx
// removed (never mutating the original, so persist's revert restores it).
func removeTriggerAt(in []fleet.Trigger, idx int) []fleet.Trigger {
	out := make([]fleet.Trigger, 0, len(in)-1)
	out = append(out, in[:idx]...)
	out = append(out, in[idx+1:]...)
	if len(out) == 0 {
		return nil
	}
	return out
}

func removeAgentAt(in []fleet.Agent, idx int) []fleet.Agent {
	out := make([]fleet.Agent, 0, len(in)-1)
	out = append(out, in[:idx]...)
	out = append(out, in[idx+1:]...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// fleetAgents returns the named fleet's automation agents (nil if none).
func fleetAgents(m *model, fleetName string) []fleet.Agent {
	if f, ok := m.st.Fleets[fleetName]; ok {
		return f.Settings.Agents
	}
	return nil
}
