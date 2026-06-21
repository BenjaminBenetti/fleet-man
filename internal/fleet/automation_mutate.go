package fleet

import (
	"fmt"
	"slices"
)

// automation_mutate.go holds the per-fleet add/edit/delete operations for
// automation Agents and Triggers (issue #189). They are the single home for the
// non-trivial invariants the three editors (TUI, CLI, MCP) all need:
//
//   - a name is unique within its list (a duplicate is ambiguous, not a no-op);
//   - renaming an agent rewrites every trigger that references it, so the
//     settings never carry a dangling agent reference;
//   - an agent still referenced by a trigger cannot be deleted (that would
//     orphan the trigger, which the server rejects wholesale).
//
// Every function takes and returns a FleetSettings by value and never mutates
// the input's Agents/Triggers slices (it builds fresh ones), so a caller that
// applies the result optimistically can revert by restoring the original
// settings. The item is normalized (NormalizeAgent / NormalizeTrigger) before
// it lands in the list, so these give the same client-side validation the
// server re-applies authoritatively in SetFleetSettings.

// FindAgent returns the agent named name (and true) from agents, or a zero
// Agent and false. The match is on the trimmed-and-normalized name space:
// names are compared verbatim, as they are stored already trimmed.
func FindAgent(agents []Agent, name string) (Agent, bool) {
	if i := indexOfAgent(agents, name); i >= 0 {
		return agents[i], true
	}
	return Agent{}, false
}

// FindTrigger returns the trigger named name (and true), or a zero Trigger and
// false.
func FindTrigger(triggers []Trigger, name string) (Trigger, bool) {
	if i := indexOfTrigger(triggers, name); i >= 0 {
		return triggers[i], true
	}
	return Trigger{}, false
}

// AddAgent appends a (normalized) agent to s, rejecting a name already in use.
func AddAgent(s FleetSettings, a Agent) (FleetSettings, error) {
	norm, err := NormalizeAgent(a)
	if err != nil {
		return s, err
	}
	if indexOfAgent(s.Agents, norm.Name) >= 0 {
		return s, fmt.Errorf("agent %q already exists", norm.Name)
	}
	s.Agents = append(append([]Agent(nil), s.Agents...), norm)
	return s, nil
}

// UpdateAgent replaces the agent named name with updated (normalized). When the
// name changes, every trigger referencing the old name is rewritten so no
// trigger is left dangling. It errors if name is absent or the new name
// collides with a different agent.
func UpdateAgent(s FleetSettings, name string, updated Agent) (FleetSettings, error) {
	idx := indexOfAgent(s.Agents, name)
	if idx < 0 {
		return s, fmt.Errorf("agent %q not found", name)
	}
	norm, err := NormalizeAgent(updated)
	if err != nil {
		return s, err
	}
	for i, a := range s.Agents {
		if i != idx && a.Name == norm.Name {
			return s, fmt.Errorf("agent %q already exists", norm.Name)
		}
	}
	newAgents := append([]Agent(nil), s.Agents...)
	oldName := newAgents[idx].Name
	newAgents[idx] = norm
	s.Agents = newAgents
	if oldName != norm.Name {
		s.Triggers = renameAgentInTriggers(s.Triggers, oldName, norm.Name)
	}
	return s, nil
}

// DeleteAgent removes the agent named name. It errors if the agent is absent or
// is still referenced by a trigger (deleting it would orphan that trigger).
func DeleteAgent(s FleetSettings, name string) (FleetSettings, error) {
	idx := indexOfAgent(s.Agents, name)
	if idx < 0 {
		return s, fmt.Errorf("agent %q not found", name)
	}
	for _, t := range s.Triggers {
		if slices.Contains(t.AgentNames, name) {
			return s, fmt.Errorf("agent %q is referenced by trigger %q; remove it from that trigger first", name, t.Name)
		}
	}
	s.Agents = removeAgentAt(s.Agents, idx)
	return s, nil
}

// AddTrigger appends a (normalized) trigger to s, rejecting a name already in
// use. The trigger is validated against s.Agents (every referenced agent must
// exist).
func AddTrigger(s FleetSettings, t Trigger) (FleetSettings, error) {
	norm, err := NormalizeTrigger(t, agentNameSet(s.Agents))
	if err != nil {
		return s, err
	}
	if indexOfTrigger(s.Triggers, norm.Name) >= 0 {
		return s, fmt.Errorf("trigger %q already exists", norm.Name)
	}
	s.Triggers = append(append([]Trigger(nil), s.Triggers...), norm)
	return s, nil
}

// UpdateTrigger replaces the trigger named name with updated (normalized
// against s.Agents). It errors if name is absent or the new name collides with
// a different trigger.
func UpdateTrigger(s FleetSettings, name string, updated Trigger) (FleetSettings, error) {
	idx := indexOfTrigger(s.Triggers, name)
	if idx < 0 {
		return s, fmt.Errorf("trigger %q not found", name)
	}
	norm, err := NormalizeTrigger(updated, agentNameSet(s.Agents))
	if err != nil {
		return s, err
	}
	for i, t := range s.Triggers {
		if i != idx && t.Name == norm.Name {
			return s, fmt.Errorf("trigger %q already exists", norm.Name)
		}
	}
	newTriggers := append([]Trigger(nil), s.Triggers...)
	newTriggers[idx] = norm
	s.Triggers = newTriggers
	return s, nil
}

// DeleteTrigger removes the trigger named name, erroring if it is absent. A
// trigger has no dependents, so there is no reference guard.
func DeleteTrigger(s FleetSettings, name string) (FleetSettings, error) {
	idx := indexOfTrigger(s.Triggers, name)
	if idx < 0 {
		return s, fmt.Errorf("trigger %q not found", name)
	}
	s.Triggers = removeTriggerAt(s.Triggers, idx)
	return s, nil
}

// indexOfAgent / indexOfTrigger return the slice index of the named item, or -1.
func indexOfAgent(agents []Agent, name string) int {
	for i, a := range agents {
		if a.Name == name {
			return i
		}
	}
	return -1
}

func indexOfTrigger(triggers []Trigger, name string) int {
	for i, t := range triggers {
		if t.Name == name {
			return i
		}
	}
	return -1
}

// agentNameSet is the lookup NormalizeTrigger consumes to resolve a trigger's
// agent references.
func agentNameSet(agents []Agent) map[string]struct{} {
	set := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		set[a.Name] = struct{}{}
	}
	return set
}

// renameAgentInTriggers returns a deep copy of triggers with every reference to
// oldName rewritten to newName (deep copy so an optimistic-revert never sees a
// mutated original).
func renameAgentInTriggers(triggers []Trigger, oldName, newName string) []Trigger {
	if len(triggers) == 0 {
		return nil
	}
	out := make([]Trigger, len(triggers))
	for i, t := range triggers {
		t.AgentNames = append([]string(nil), t.AgentNames...)
		for j, an := range t.AgentNames {
			if an == oldName {
				t.AgentNames[j] = newName
			}
		}
		out[i] = t
	}
	return out
}

// removeAgentAt / removeTriggerAt return a NEW slice with the element at idx
// removed (never mutating the original), nil when the result is empty so the
// persisted JSON stays clean (matching the `,omitempty` list tags).
func removeAgentAt(in []Agent, idx int) []Agent {
	out := make([]Agent, 0, len(in)-1)
	out = append(out, in[:idx]...)
	out = append(out, in[idx+1:]...)
	if len(out) == 0 {
		return nil
	}
	return out
}

func removeTriggerAt(in []Trigger, idx int) []Trigger {
	out := make([]Trigger, 0, len(in)-1)
	out = append(out, in[:idx]...)
	out = append(out, in[idx+1:]...)
	if len(out) == 0 {
		return nil
	}
	return out
}
