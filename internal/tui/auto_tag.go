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

// prSignalStyle maps a PrSignal colour to its lipgloss style (PR indicator).
func prSignalStyle(sig fleetgrpc.PrSignal) lipgloss.Style {
	switch sig {
	case fleetgrpc.PrSignal_PR_SIGNAL_GREEN:
		return prGreenStyle
	case fleetgrpc.PrSignal_PR_SIGNAL_RED:
		return prRedStyle
	case fleetgrpc.PrSignal_PR_SIGNAL_PURPLE:
		return prPurpleStyle
	default:
		return prYellowStyle
	}
}

// prChecksStyle is like prSignalStyle but de-emphasises the pending (yellow)
// state to grey, since pending checks are the noisiest, least-actionable state.
func prChecksStyle(sig fleetgrpc.PrSignal) lipgloss.Style {
	switch sig {
	case fleetgrpc.PrSignal_PR_SIGNAL_GREEN:
		return prGreenStyle
	case fleetgrpc.PrSignal_PR_SIGNAL_RED:
		return prRedStyle
	default: // YELLOW (pending) / UNSPECIFIED
		return prGrayStyle
	}
}

// instanceAutoTag renders the auto tag for an instance, or "" when there is
// nothing to show — gh is unavailable inside the instance, the branch never had a
// PR, or the status hasn't been probed yet. The format is "[PR|PRxN] [Rejected |
// Accepted | Pending] [Checks x/x]", each element coloured and shown only when it
// applies. When the only PR(s) are closed or merged, it shrinks to a single
// purple "PR" badge (no review/checks) that persists so a finished instance stays
// distinguishable from one that never had a PR.
//
// When selected (horizontally, via →/l), the whole tag is drawn as one chunk in
// the pink selection colour wrapped in [ ], so it reads as a single navigable
// unit rather than three separately-coloured indicators.
func (m *model) instanceAutoTag(fleetName, instance string, selected bool) string {
	ps := m.runtime[rtKey(fleetName, instance)].GetPrStatus()
	if ps == nil || (ps.GetOpenCount() == 0 && ps.GetClosedCount() == 0) {
		return ""
	}

	type segment struct {
		text  string
		style lipgloss.Style
	}
	segments := make([]segment, 0, 3)

	// PR indicator: "PR" for a single open PR, "PRxN" for N>1.
	label := "PR"
	if n := ps.GetOpenCount(); n > 1 {
		label = fmt.Sprintf("PRx%d", n)
	}
	segments = append(segments, segment{label, prSignalStyle(ps.GetPrSignal())})

	// Review indicator: one of the three concrete states for an open PR. Pending
	// is grey to keep the in-progress state quiet.
	switch ps.GetReview() {
	case fleetgrpc.PrReviewState_PR_REVIEW_STATE_CHANGES_REQUESTED:
		segments = append(segments, segment{"Rejected", prRedStyle})
	case fleetgrpc.PrReviewState_PR_REVIEW_STATE_APPROVED:
		segments = append(segments, segment{"Accepted", prGreenStyle})
	case fleetgrpc.PrReviewState_PR_REVIEW_STATE_PENDING:
		segments = append(segments, segment{"Pending", prGrayStyle})
	}

	// Checks indicator: only when the PRs have any checks. Pending checks render
	// grey (prChecksStyle) rather than yellow to reduce noise.
	if ps.GetChecksTotal() > 0 {
		segments = append(segments, segment{
			fmt.Sprintf("Checks %d/%d", ps.GetChecksPassed(), ps.GetChecksTotal()),
			prChecksStyle(ps.GetChecksSignal()),
		})
	}

	if selected {
		plain := make([]string, len(segments))
		for i, s := range segments {
			plain[i] = s.text
		}
		return selectedStyle.Render("[" + strings.Join(plain, "  ") + "]")
	}

	parts := make([]string, len(segments))
	for i, s := range segments {
		parts[i] = s.style.Render(s.text)
	}
	return strings.Join(parts, "  ")
}
