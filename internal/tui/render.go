package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/lipgloss"
)

func renderHelp(width int, helpKeys []string) string {
	maxW := width
	if maxW <= 0 {
		maxW = 80
	}
	var helpLines []string
	var cur string
	for _, k := range helpKeys {
		entry := k
		if cur != "" {
			entry = "  " + k
		}
		if cur != "" && len(cur)+len(entry) > maxW {
			helpLines = append(helpLines, cur)
			cur = k
		} else {
			cur += entry
		}
	}
	if cur != "" {
		helpLines = append(helpLines, cur)
	}
	return helpStyle.Render(strings.Join(helpLines, "\n")) + "\n"
}

func renderStatus(s fleet.InstanceStatus) string {
	switch s {
	case fleet.StatusRunning:
		return statusRunningStyle.Render("running")
	case fleet.StatusStopped:
		return statusStoppedStyle.Render("stopped")
	case fleet.StatusCreating:
		return statusCreatingStyle.Render("creating")
	case fleet.StatusCloning:
		return statusCreatingStyle.Render("cloning")
	case fleet.StatusStopping:
		return statusCreatingStyle.Render("stopping")
	case fleet.StatusStarting:
		return statusCreatingStyle.Render("starting")
	case fleet.StatusDeleting:
		return statusCreatingStyle.Render("deleting")
	case fleet.StatusFailed:
		return errorStyle.Render("failed")
	default:
		return dimStyle.Render(string(s))
	}
}

// isTransitional returns true for statuses that indicate an in-progress
// background operation (shown with a spinner on the instance row).
func isTransitional(s fleet.InstanceStatus) bool {
	switch s {
	case fleet.StatusCreating, fleet.StatusCloning, fleet.StatusStopping, fleet.StatusStarting, fleet.StatusDeleting:
		return true
	}
	return false
}

// agentToolLabel returns a human-readable label for the given agent tool.
// agentToolLabelProto maps the proto AgentTool enum from the
// server's runtime sidecar. The label is only rendered for a detected, active
// agent; UNSPECIFIED/NONE fall through to the default.
func agentToolLabelProto(tool fleetgrpc.AgentTool) string {
	switch tool {
	case fleetgrpc.AgentTool_AGENT_TOOL_CODEX:
		return "Codex"
	case fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE:
		return "Claude Code"
	case fleetgrpc.AgentTool_AGENT_TOOL_GEMINI:
		return "Gemini"
	case fleetgrpc.AgentTool_AGENT_TOOL_COPILOT:
		return "Copilot"
	default:
		return "Claude Code"
	}
}

func renderGradient(text string) string {
	// Gradient from light cyan to deep blue
	type rgb struct{ r, g, b float64 }
	from := rgb{130, 220, 255}
	to := rgb{60, 80, 200}

	lines := strings.Split(text, "\n")
	// Find max line length for consistent gradient
	maxLen := 0
	for _, line := range lines {
		if len(line) > maxLen {
			maxLen = len(line)
		}
	}
	if maxLen == 0 {
		return text
	}

	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteString("\n")
		}
		for j, ch := range line {
			if ch == ' ' {
				out.WriteRune(ch)
				continue
			}
			t := float64(j) / float64(maxLen)
			r := int(from.r + (to.r-from.r)*t)
			g := int(from.g + (to.g-from.g)*t)
			b := int(from.b + (to.b-from.b)*t)
			color := fmt.Sprintf("#%02x%02x%02x", r, g, b)
			out.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color)).Render(string(ch)))
		}
	}
	return out.String()
}
