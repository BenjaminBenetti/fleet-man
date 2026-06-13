package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"github.com/charmbracelet/lipgloss"
)

// versionArrow separates hops in the control-chain version string.
const versionArrow = " → "

// versionChain renders the build versions of every hop in the control chain so
// version skew (a stale TUI, gateway, or daemon) is visible at a glance:
//
//   - local daemon, versions match (the common case — fleetd auto-upgrades to
//     the client): just "<tui>" (unchanged from the old single-version header;
//     a dev build with no version still renders nothing).
//   - local daemon, versions differ: "<tui> → <fleetd>".
//   - remote via a gateway: "<tui> → <gateway> → <fleetd>".
//   - remote via FLEET_SERVER (no gateway): "<tui> → <fleetd>".
//
// An empty gateway version (an old gateway predating the version field, or the
// status not yet received) renders as "?" so the unknown hop stands out rather
// than being masked.
func versionChain(m *model) string {
	switch {
	case fleetclient.IsGateway():
		gw := m.remoteMcpStatus.GetGatewayVersion()
		if gw == "" {
			gw = "?"
		}
		return versionLabel(version.Version) + versionArrow + gw + versionArrow + versionLabel(m.serverVersion)
	case fleetclient.IsRemote():
		return versionLabel(version.Version) + versionArrow + versionLabel(m.serverVersion)
	default:
		// Local daemon. Collapse to the TUI version alone when the daemon
		// matches (or hasn't answered yet) — preserving the old header exactly,
		// including rendering nothing for a versionless dev build — and only
		// expand to show the daemon version when they actually diverge.
		if m.serverVersion == "" || m.serverVersion == version.Version {
			return version.Version
		}
		return versionLabel(version.Version) + versionArrow + versionLabel(m.serverVersion)
	}
}

// versionLabel maps an empty (unset, dev-build) version to "dev" so a chain hop
// always renders a token.
func versionLabel(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

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

// remoteIndicator returns a small "wifi"-style signal glyph that radiates off
// the end of the fleet logo when Fleet Remote (the outbound MCP gateway tunnel)
// is enabled. It is green once the tunnel is CONNECTED and red while enabled but
// not yet up (connecting / error). When remote is disabled it returns "" so the
// header renders unchanged.
func remoteIndicator(m *model) string {
	if m.config == nil || !m.config.RemoteMcpSettings.Enabled {
		return ""
	}
	style := errorStyle
	if m.remoteMcpStatus != nil &&
		m.remoteMcpStatus.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
		style = statusRunningStyle
	}
	return style.Render("·))")
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
