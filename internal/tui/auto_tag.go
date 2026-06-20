package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/charmbracelet/lipgloss"
)

// auto_tag.go renders the "auto tag": a glanceable PR/review/checks status line
// the TUI shows in an instance's tag slot when the instance has no user-set tag.
// The underlying data (fleetgrpc.PrStatus) is computed server-side by running
// `gh` inside the instance (see internal/server/pr_status.go) and pushed over
// the runtime sidecar; this file only formats it.

// prSignalStyle maps a PrSignal colour to its lipgloss style.
func prSignalStyle(sig fleetgrpc.PrSignal) lipgloss.Style {
	switch sig {
	case fleetgrpc.PrSignal_PR_SIGNAL_GREEN:
		return prGreenStyle
	case fleetgrpc.PrSignal_PR_SIGNAL_RED:
		return prRedStyle
	default:
		return prYellowStyle
	}
}

// instanceAutoTag renders the auto tag for an instance, or "" when there is
// nothing to show — gh is unavailable inside the instance, no PR is open, or the
// status hasn't been probed yet. The format is "[PR|PRxN] [Changes Requested |
// Approved] [Checks x/x]", each element coloured and shown only when it applies.
func (m *model) instanceAutoTag(fleetName, instance string) string {
	ps := m.runtime[rtKey(fleetName, instance)].GetPrStatus()
	if ps == nil || ps.GetOpenCount() == 0 {
		return ""
	}

	parts := make([]string, 0, 3)

	// PR indicator: "PR" for a single open PR, "PRxN" for N>1.
	label := "PR"
	if n := ps.GetOpenCount(); n > 1 {
		label = fmt.Sprintf("PRx%d", n)
	}
	parts = append(parts, prSignalStyle(ps.GetPrSignal()).Render(label))

	// Review indicator: only when a decision has landed.
	switch ps.GetReview() {
	case fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED:
		parts = append(parts, prRedStyle.Render("Changes Requested"))
	case fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED:
		parts = append(parts, prGreenStyle.Render("Approved"))
	}

	// Checks indicator: only when the PRs have any checks.
	if ps.GetChecksTotal() > 0 {
		checks := fmt.Sprintf("Checks %d/%d", ps.GetChecksPassed(), ps.GetChecksTotal())
		parts = append(parts, prSignalStyle(ps.GetChecksSignal()).Render(checks))
	}

	return strings.Join(parts, "  ")
}
