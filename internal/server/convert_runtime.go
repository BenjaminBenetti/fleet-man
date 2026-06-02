package server

import (
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// convert_runtime.go maps the live backend/agent-detection types into the
// fleetgrpc runtime sidecar enums/messages.

func liveStatusToProto(s backend.LiveStatus) fleetgrpc.LiveContainerStatus {
	switch s {
	case backend.LiveStatusRunning:
		return fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING
	case backend.LiveStatusStopped:
		return fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_STOPPED
	case backend.LiveStatusMissing:
		return fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_MISSING
	case backend.LiveStatusUnknown:
		return fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_UNKNOWN
	default:
		return fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_UNSPECIFIED
	}
}

func agentToolToProto(t state.AgentTool) fleetgrpc.AgentTool {
	switch t {
	case "":
		return fleetgrpc.AgentTool_AGENT_TOOL_NONE
	case state.AgentToolClaude:
		return fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE
	case state.AgentToolCodex:
		return fleetgrpc.AgentTool_AGENT_TOOL_CODEX
	case state.AgentToolGemini:
		return fleetgrpc.AgentTool_AGENT_TOOL_GEMINI
	case state.AgentToolCopilot:
		return fleetgrpc.AgentTool_AGENT_TOOL_COPILOT
	default:
		return fleetgrpc.AgentTool_AGENT_TOOL_UNSPECIFIED
	}
}

func agentActivityToProto(s agentdetect.State) fleetgrpc.AgentActivity {
	switch s {
	case agentdetect.StateWorking:
		return fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING
	case agentdetect.StateWaiting:
		return fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING
	case agentdetect.StateNotRunning:
		return fleetgrpc.AgentActivity_AGENT_ACTIVITY_NOT_RUNNING
	default:
		return fleetgrpc.AgentActivity_AGENT_ACTIVITY_UNSPECIFIED
	}
}

func statsToProto(s *backend.ContainerStats) *fleetgrpc.ContainerStats {
	if s == nil {
		return nil
	}
	return &fleetgrpc.ContainerStats{CpuMillicores: s.CPUMillicores, MemoryMb: s.MemoryMB}
}

// parseTmuxSessionsProto parses the output of
// `tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}"`
// into proto TmuxSessions (server-side mirror of the TUI's parseTmuxSessions).
func parseTmuxSessionsProto(output string) []*fleetgrpc.TmuxSession {
	var sessions []*fleetgrpc.TmuxSession
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 1 || parts[0] == "" {
			continue
		}
		s := &fleetgrpc.TmuxSession{Name: parts[0]}
		if len(parts) >= 2 {
			w, _ := strconv.Atoi(parts[1])
			s.Windows = int32(w)
		}
		if len(parts) >= 3 {
			s.Attached = parts[2] == "1"
		}
		sessions = append(sessions, s)
	}
	return sessions
}
