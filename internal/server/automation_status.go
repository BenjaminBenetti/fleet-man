package server

import (
	"context"
	"sort"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// automation_status.go publishes the scheduler's view of the automation
// agents over Watch (Event.automation_status): which ones the login check
// refuses (the TUI's agent row says so and offers Log in), and how many runs
// each has queued and running. The scheduler publishes it after each look
// at its watch list and queues; the hub drops an unchanged one.

// automationStatus is the status of every agent of st with something to
// say, in fleet then agent order.
func (s *service) automationStatus(lim *automationLimiter, st *state.State) *fleetgrpc.AutomationStatus {
	out := &fleetgrpc.AutomationStatus{}
	queued, running := lim.counts()
	seen := map[string]bool{}
	c := s.runChecks()
	if st != nil {
		for fleetName, f := range st.Fleets {
			if f == nil {
				continue
			}
			for _, ag := range f.Settings.Agents {
				key := agentKey(fleetName, ag.Name)
				seen[key] = true
				row := &fleetgrpc.AutomationAgentStatus{
					Fleet: fleetName, Agent: ag.Name,
					Queued: int32(queued[key]), Running: int32(running[key]),
				}
				if target := ag.DeployTarget(); target.Backend == fleet.BackendKubernetes {
					// A forgotten connection shows as (missing) instead.
					if lim, ok := c.limits(target.KubeCluster); ok {
						why, tool := loginRefusal(st, fleetName, ag, lim.name)
						row.Login, row.LoginTool = why, tool.Tool
					}
				}
				if row.Login != "" || row.Queued > 0 || row.Running > 0 {
					out.Agents = append(out.Agents, row)
				}
			}
		}
	}
	// Runs of an agent that was deleted or renamed meanwhile still count.
	for _, counts := range []map[string]int{queued, running} {
		for key := range counts {
			if seen[key] {
				continue
			}
			seen[key] = true
			fleetName, agentName := splitAgentKey(key)
			out.Agents = append(out.Agents, &fleetgrpc.AutomationAgentStatus{
				Fleet: fleetName, Agent: agentName,
				Queued: int32(queued[key]), Running: int32(running[key]),
			})
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		a, b := out.Agents[i], out.Agents[j]
		if a.GetFleet() != b.GetFleet() {
			return a.GetFleet() < b.GetFleet()
		}
		return a.GetAgent() < b.GetAgent()
	})
	return out
}

// GetAutomationStatus is the automation status as of the call, of one
// fleet's agents or every fleet's: what Watch pushes, for a client that does
// not watch (`fleet agent list`, the MCP automation list).
func (s *service) GetAutomationStatus(_ context.Context, req *fleetgrpc.GetAutomationStatusRequest) (*fleetgrpc.GetAutomationStatusReply, error) {
	st := loadScheduleState()
	if name := req.GetFleet(); name != "" && (st == nil || st.Fleets[name] == nil) {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", name)
	}
	return &fleetgrpc.GetAutomationStatusReply{Status: s.fleetAutomationStatus(st, req.GetFleet())}, nil
}

// fleetAutomationStatus is automationStatus of st's agents, only fleetName's
// unless it is empty.
func (s *service) fleetAutomationStatus(st *state.State, fleetName string) *fleetgrpc.AutomationStatus {
	all := s.automationStatus(s.limiter(), st)
	if fleetName == "" {
		return all
	}
	out := &fleetgrpc.AutomationStatus{}
	for _, a := range all.GetAgents() {
		if a.GetFleet() == fleetName {
			out.Agents = append(out.Agents, a)
		}
	}
	return out
}

// publishAutomationStatus pushes the current automation status over Watch.
func (s *service) publishAutomationStatus(sched *scheduler) {
	if s.hub == nil {
		return
	}
	snapshot := s.automationStatus(sched.lim, loadScheduleState())
	s.hub.post(func(h *hub) { h.broadcastAutomationStatus(snapshot) })
}
