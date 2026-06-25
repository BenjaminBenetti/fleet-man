package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// auto_tag_nav.go makes the auto tag interactive: horizontally selecting an
// instance's PR status (→/l) and opening the PR in a browser (enter, or a
// click), with a chooser dialog when more than one PR is open.

// instancePRRefs returns the PRs backing an instance's auto tag — the open PRs,
// or the closed/merged ones when none are open (nil when there is no PR status).
func (m *model) instancePRRefs(fleetName, instance string) []*fleetgrpc.PrRef {
	return m.runtime[rtKey(fleetName, instance)].GetPrStatus().GetPrs()
}

// rowInlinePRRefs returns the PRs for a row that carries the inline PR status
// (the first child row of an expanded, untagged instance), or nil. This is where
// the auto tag lives now, so it is the unit the cursor selects.
func (m *model) rowInlinePRRefs(r row) []*fleetgrpc.PrRef {
	if !r.prStatusInline || r.instance == nil {
		return nil
	}
	return m.instancePRRefs(r.fleetName, r.instance.Name)
}

// selectedInlinePR returns the PRs of the inline PR status under the cursor, or
// nil when the cursor isn't on a row carrying one.
func (fleetPage *fleetPage) selectedInlinePR(m *model) []*fleetgrpc.PrRef {
	r := fleetPage.currentRow()
	if r == nil {
		return nil
	}
	return m.rowInlinePRRefs(*r)
}

// openSelectedPR opens the PR for the inline status under the cursor — directly
// when there is one, or via the chooser dialog when there are several.
func (fleetPage *fleetPage) openSelectedPR(m *model) tea.Cmd {
	refs := fleetPage.selectedInlinePR(m)
	switch len(refs) {
	case 0:
		return nil
	case 1:
		return openPRRefCmd(m, refs[0])
	default:
		fleetPage.choosePR = choosePRState{refs: refs, cursor: 0}
		fleetPage.mode = viewChoosePR
		return nil
	}
}

// openPRRefCmd opens one PR ref and notes it in the status line.
func openPRRefCmd(m *model, ref *fleetgrpc.PrRef) tea.Cmd {
	if ref.GetUrl() == "" {
		return nil
	}
	m.message = fmt.Sprintf("Opening PR #%d…", ref.GetNumber())
	return openExternalURLCmd(ref.GetUrl())
}

// ===========================================
// Multi-PR chooser dialog
// ===========================================

// choosePRState backs the "which PR?" dialog shown when an instance has more
// than one PR (open, or closed/merged).
type choosePRState struct {
	refs   []*fleetgrpc.PrRef
	cursor int
}

// updateChoosePR handles the multi-PR chooser dialog.
func (fleetPage *fleetPage) updateChoosePR(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	n := len(fleetPage.choosePR.refs)
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		return nil
	case "up", "k":
		if n > 0 {
			fleetPage.choosePR.cursor = (fleetPage.choosePR.cursor - 1 + n) % n
		}
	case "down", "j":
		if n > 0 {
			fleetPage.choosePR.cursor = (fleetPage.choosePR.cursor + 1) % n
		}
	case "enter", " ":
		if c := fleetPage.choosePR.cursor; c >= 0 && c < n {
			ref := fleetPage.choosePR.refs[c]
			fleetPage.mode = viewNormal
			return openPRRefCmd(m, ref)
		}
		fleetPage.mode = viewNormal
	}
	return nil
}

// renderChoosePRDialog draws the chooser: one row per PR (number, title, url),
// with the selected row highlighted.
func (fleetPage *fleetPage) renderChoosePRDialog(m *model) string {
	var b strings.Builder
	b.WriteString(dialogTitle.Render("Open which PR?"))
	b.WriteString("\n\n")

	for i, ref := range fleetPage.choosePR.refs {
		cursor := "  "
		title := fmt.Sprintf("#%d  %s", ref.GetNumber(), ref.GetTitle())
		url := ref.GetUrl()
		if i == fleetPage.choosePR.cursor {
			cursor = cursorStyle.Render("> ")
			title = selectedStyle.Render(title)
		} else {
			title = dialogLabel.Render(title)
		}
		line := cursor + title
		if maxW := 60; lipgloss.Width(line) > maxW {
			line = ansi.Truncate(line, maxW, "…")
		}
		b.WriteString(line)
		b.WriteString("\n")
		b.WriteString("    " + dimStyle.Render(ansi.Truncate(url, 60, "…")))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(dialogHint.Render("↑/↓ select · enter open · esc cancel"))
	return dialogBox.Render(b.String())
}
