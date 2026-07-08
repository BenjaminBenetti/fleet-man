package cli

import (
	"context"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// automation.go is the shared plumbing for the `fleet agent` and `fleet trigger`
// command trees (issue #189): programmatic CRUD over a fleet's automation
// agents and triggers (the same config the TUI's automation view edits).
//
// There is no per-item RPC — automation lives inline on FleetSettings, which
// SetFleetSettings replaces wholesale. So every write is read-modify-write:
// fetch the fleet's current settings, apply one change to the agent/trigger
// lists (via the shared fleet.* mutators, which own the invariants — unique
// names, rename-rewrites-references, no-orphan deletes), and send the full
// settings back. The other settings fields ride along untouched on the proto
// object we read, so a write here never disturbs mounts, presets, or the rest.
//
// loadAutomation / mutateAutomation are package vars so the command tests can
// stub the server round-trip and exercise the command + mutation logic in
// memory.

// loadAutomation fetches the named fleet's automation lists as domain types.
var loadAutomation = func(ctx context.Context, fleetName string) (fleet.FleetSettings, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	defer conn.Close()
	reply, err := conn.Service().GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return fleet.FleetSettings{}, err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fleet.FleetSettings{}, fmt.Errorf("fleet %q not found", fleetName)
	}
	ps := pf.GetSettings()
	return fleet.FleetSettings{
		Agents:   protoconv.AgentsFromProto(ps.GetAgents()),
		Triggers: protoconv.TriggersFromProto(ps.GetTriggers()),
	}, nil
}

// triggerLogs fetches a trigger's recorded event logs (its recent firings'
// payloads, concatenated) from the daemon. A package var so the command test can
// stub the server round-trip.
var triggerLogs = func(ctx context.Context, fleetName, triggerName string) (string, error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	reply, err := conn.Service().TriggerLogs(ctx, &fleetgrpc.TriggerLogsRequest{Fleet: fleetName, Trigger: triggerName})
	if err != nil {
		return "", err
	}
	return reply.GetLogs(), nil
}

// mutateAutomation reads the named fleet's settings, applies fn to its
// (domain) agent/trigger lists, and writes the result back — preserving every
// other settings field. fn returns the modified settings (the shared fleet.*
// mutators do exactly this) or an error that aborts the write.
var mutateAutomation = func(ctx context.Context, fleetName string, fn func(fleet.FleetSettings) (fleet.FleetSettings, error)) error {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	svc := conn.Service()

	reply, err := svc.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return err
	}
	pf := reply.GetState().GetFleets()[fleetName]
	if pf == nil {
		return fmt.Errorf("fleet %q not found", fleetName)
	}
	// Keep the live proto settings as the carrier so all the fields this command
	// doesn't touch (mounts, presets, home dir, ...) survive the wholesale write.
	ps := pf.GetSettings()
	if ps == nil {
		ps = &fleetgrpc.FleetSettings{}
	}

	settings := fleet.FleetSettings{
		Agents:   protoconv.AgentsFromProto(ps.GetAgents()),
		Triggers: protoconv.TriggersFromProto(ps.GetTriggers()),
	}
	settings, err = fn(settings)
	if err != nil {
		return err
	}
	ps.Agents = protoconv.AgentsToProto(settings.Agents)
	ps.Triggers = protoconv.TriggersToProto(settings.Triggers)

	if _, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{Fleet: fleetName, Settings: ps}); err != nil {
		return err
	}
	return nil
}
