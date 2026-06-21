package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	// instanceNameWidth is the fixed cell width of an instance's name on its row.
	// The name is padded to this width — and truncated with an ellipsis when it
	// runs longer — so the status column never shifts. That keeps the PR-status
	// "second status line" (see the rowInstanceTag render) lined up directly under
	// the main status word regardless of how long the instance name is.
	instanceNameWidth = 22

	// instanceAutoMarkWidth is the cell width the trailing automation marker
	// (" ⟳", issue #188) occupies INSIDE the fixed-width name column on an
	// automation-spawned instance row. The name is truncated this much shorter to
	// make room, so the marker never widens the column or shifts the status word —
	// and user-created rows keep their original, un-indented position.
	instanceAutoMarkWidth = 2

	// instanceStatusCol is the column where the status word starts on an instance
	// row, and the indent the PR-status second line is rendered at so it sits
	// under that status. It is the sum of the row's fixed prefix cells: cursor(2)
	// + gap(4) + arrow(2) + throbber(1) + gap(1) + name + gap(1).
	instanceStatusCol = 2 + 4 + 2 + 1 + 1 + instanceNameWidth + 1

	// sessionLabelCol is the column an instance child row's label starts at:
	// cursor(2) + the 8-space child indent (the "%s        %s" child-row format).
	sessionLabelCol = 2 + 8

	// sessionInlineLabelWidth is the label field width on the first child row when
	// it also carries the inline PR status. Sized (with a 1-space gap) so the PR
	// status lands at instanceStatusCol — directly under the instance status. It
	// equals instanceNameWidth because a child label and an instance name happen
	// to start at the same column.
	sessionInlineLabelWidth = instanceStatusCol - sessionLabelCol - 1

	// listContentXOffset is the terminal column where a list-box content line's
	// text begins: the box's left border (1) + horizontal padding (1).
	listContentXOffset = 2

	// prStatusClickColumn is the absolute terminal column where the inline PR
	// status begins, used to tell a click on the PR status apart from a click on
	// the child row's session label.
	prStatusClickColumn = listContentXOffset + instanceStatusCol

	// automationMark is the glyph shown before an automation-spawned instance's
	// name (issue #188) — a clockwise loop reading as "runs on a schedule".
	automationMark = "⟳"
)

// renderChildRowLine renders an instance child row (a session/group row or the
// "+ new session" row): the cursor, the 8-space child indent, and the styled
// label. When the row carries the inline PR status (the first child of an
// expanded instance with no user tag), the label is truncated with an ellipsis
// to a fixed width and the instance's PR-status auto tag is appended at the
// status column — a "second status line" that lines up under the instance status
// and that a long label can never overrun. prSelected draws that PR status as
// the pink, bracketed selection. The returned line has no trailing newline;
// callers append it.
func (m *model) renderChildRowLine(cursor, label string, style lipgloss.Style, r row, prSelected bool) string {
	if r.prStatusInline {
		if pr := m.instanceAutoTag(r.fleetName, r.instance.Name, prSelected); pr != "" {
			field := ansi.Truncate(label, sessionInlineLabelWidth, "…")
			// Pad the (possibly truncated) label out to its field width, plus a
			// one-space separator, so the PR status starts at instanceStatusCol.
			gap := sessionInlineLabelWidth - lipgloss.Width(field) + 1
			line := fmt.Sprintf("%s        %s%s%s", cursor, style.Render(field), strings.Repeat(" ", gap), pr)
			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}
			return line
		}
	}
	return fmt.Sprintf("%s        %s", cursor, style.Render(label))
}

// renderArmadaBorder draws the list box's top border line with the Armada
// selector embedded: ╭─ Armada [ local ] ───╮. The line is hand-composed to
// the box's exact rendered width (lipgloss has no border-title API) in the
// box's border color; the label's column span is recorded for mouse
// hit-testing. Falls back to a plain border when the box is too narrow.
func (fleetPage *fleetPage) renderArmadaBorder(m *model, width int) string {
	borderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	name := m.armadaCurrentDisplay()
	frame := " Armada [  ] "
	// Corners + 2 leading dashes + at least 2 trailing dashes must survive.
	maxName := width - 6 - lipgloss.Width(frame)
	if maxName < 1 {
		fleetPage.armadaSel.x0, fleetPage.armadaSel.x1 = -1, -1
		return borderStyle.Render("╭" + strings.Repeat("─", max(0, width-2)) + "╮")
	}
	if lipgloss.Width(name) > maxName {
		name = ansi.Truncate(name, maxName, "…")
	}
	// labelWidth drives layout + mouse hit-testing, so measure the PLAIN text;
	// the ANSI styling applied below doesn't change the visible width.
	label := " Armada [ " + name + " ] "
	labelWidth := lipgloss.Width(label)

	// "Armada" wears the same light-cyan→deep-blue gradient as the "fleet" logo
	// header. The brackets + current-connection name follow the border colour,
	// switching to the selection highlight while the selector is focused / open.
	rest := " [ " + name + " ] "
	restStyle := borderStyle
	if fleetPage.armadaSel.focused || fleetPage.mode == viewArmadaSelect {
		restStyle = selectedStyle
	}
	styledLabel := " " + renderGradient("Armada") + restStyle.Render(rest)

	fleetPage.armadaSel.x0 = 3
	fleetPage.armadaSel.x1 = 3 + labelWidth
	rightDashes := max(0, width-4-labelWidth)
	return borderStyle.Render("╭──") + styledLabel + borderStyle.Render(strings.Repeat("─", rightDashes)+"╮")
}

// viewFleetList renders the fleet list page.
func (fleetPage *fleetPage) viewFleetList(m *model) string {
	var b strings.Builder

	logo := "" +
		"  __ _         _\n" +
		" / _| |___ ___| |_\n" +
		"|  _| / -_) -_)  _|\n" +
		"|_| |_\\___\\___|\\___|"
	rendered := renderGradient(logo)
	if ind := remoteIndicator(m); ind != "" {
		// Float the signal glyph up-and-right off the top of the "t" by
		// appending it to the logo's first line.
		lines := strings.Split(rendered, "\n")
		lines[0] = lines[0] + "    " + ind
		rendered = strings.Join(lines, "\n")
	}
	b.WriteString(rendered)
	if chain := versionChain(m); chain != "" {
		b.WriteString(" " + dimStyle.Render(chain))
	}
	if m.updateAvailable != "" {
		b.WriteString("  " + updateStyle.Render(fmt.Sprintf("A new version: %s is available ⚡ Settings to update", m.updateAvailable)))
	}
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	var listContent strings.Builder

	if m.st == nil || len(m.st.Fleets) == 0 {
		listContent.WriteString(dimStyle.Render("  No instances. Press 'a' to create one, or use 'fleet up <name>'."))
		listContent.WriteString("\n")
	}

	for i, r := range fleetPage.rows {
		// While the Armada selector holds focus, no row shows the cursor.
		isSelected := i == fleetPage.cursor && !fleetPage.armadaSel.focused
		cursor := "  "
		if isSelected {
			cursor = cursorStyle.Render("> ")
		}

		if r.kind == rowFleetHeader {
			arrow := "▼ "
			style := fleetExpandedStyle
			if fleetPage.collapsed[r.fleetName] {
				arrow = "▶ "
				style = fleetCollapsedStyle
			}

			// The fleet header carries a mode-toggle "button" (issue #188): the
			// instance view shows the instance count plus an [automations] switch;
			// the automation view shows an [instances] switch. 'm' (or a click on
			// the button — see app.go) toggles it. We record the button's absolute
			// column span here so the click handler can hit-test it.
			var suffix, toggleText string
			// The toggle button highlights (pink) when the header's right-hand
			// element is selected via →/l, mirroring the inline PR status; the
			// instance count stays dim either way.
			toggleStyle := dimStyle
			if isSelected && fleetPage.rightSelected {
				toggleStyle = selectedStyle
			}
			// Columns before the suffix: cursor(2) + arrow(2) + name width.
			labelStart := listContentXOffset + 4 + lipgloss.Width(r.fleetName)
			if fleetPage.automationMode[r.fleetName] {
				toggleText = "[instances]"
				suffix = dimStyle.Render(" ") + toggleStyle.Render(toggleText)
				labelStart += 1 // a single leading space precedes the label
			} else {
				count := 0
				if f, ok := m.st.Fleets[r.fleetName]; ok {
					count = len(f.Instances)
				}
				countPart := fmt.Sprintf(" (%d) ", count)
				toggleText = "[automations]"
				suffix = dimStyle.Render(countPart) + toggleStyle.Render(toggleText)
				labelStart += lipgloss.Width(countPart)
			}
			fleetPage.rows[i].toggleX0 = labelStart
			fleetPage.rows[i].toggleX1 = labelStart + lipgloss.Width(toggleText)

			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s%s%s",
					cursor,
					selectedStyle.Render(arrow+r.fleetName),
					suffix,
				))
			} else {
				listContent.WriteString(fmt.Sprintf("%s%s%s%s",
					cursor,
					style.Render(arrow),
					style.Render(r.fleetName),
					suffix,
				))
			}
			listContent.WriteString("\n")
		} else if r.kind == rowSession {
			icon := "○"
			style := sessionStyle
			displayGroup := fleetPage.split.activeGroup
			if !fleetPage.split.pendingGroup.Empty() {
				displayGroup = fleetPage.split.pendingGroup
			}
			rowGroup := ActiveGroup{
				Ref:     InstanceRef{Fleet: r.fleetName, Instance: r.instance.Name},
				GroupID: r.groupID,
			}
			if r.groupID != "" && rowGroup == displayGroup {
				icon = "●"
				style = sessionActiveStyle
			}
			var label string
			if r.groupSize > 1 {
				label = fmt.Sprintf("%s %s (%d panes)", icon, r.groupID, r.groupSize)
			} else if r.groupID != "" && isGroupedSession(SanitizeSessionName(r.instance.Name), r.sessionName) {
				label = fmt.Sprintf("%s %s", icon, r.groupID)
			} else {
				label = fmt.Sprintf("%s %s", icon, r.sessionName)
			}
			if isSelected {
				style = selectedStyle
			}
			listContent.WriteString(m.renderChildRowLine(cursor, label, style, r, isSelected && fleetPage.rightSelected))
			listContent.WriteString("\n")

		} else if r.kind == rowNewSession {
			label := "+ new session"
			style := newSessionStyle
			if isSelected {
				style = selectedStyle
			}
			listContent.WriteString(m.renderChildRowLine(cursor, label, style, r, isSelected && fleetPage.rightSelected))
			listContent.WriteString("\n")

		} else if r.kind == rowInstance {
			instance := r.instance

			transitional := isTransitional(instance.Status)
			var status string
			if transitional {
				status = strings.TrimRight(m.spinner.View(), "\n") + " " + statusCreatingStyle.Render(string(instance.Status))
			} else {
				status = renderStatus(instance.Status)
			}

			ref := InstanceRef{Fleet: r.fleetName, Instance: instance.Name}
			arrow := "  "
			if instance.Status == fleet.StatusRunning {
				if m.sessionStore.IsExpanded(ref) {
					arrow = "▼ "
				} else {
					arrow = "▶ "
				}
			}

			// Single switch derives BOTH the left-of-name throbber
			// and the right-of-status agentStr. Colors mirror the
			// right indicator: green animated pulse while working,
			// yellow static ○ while the agent is alive but waiting,
			// grey ○ when the agent is absent (or instance not
			// running). agentStr is consumed below in the
			// non-transitional render branch.
			throbber := agentOffStyle.Render("○")
			agentStr := ""
			if instance.Status == fleet.StatusRunning {
				// Live agent state comes from the server's runtime sidecar
				// (P2 Step 7). A missing entry / UNSPECIFIED activity renders as
				// "○ idle" (same as NOT_RUNNING), so a running row shows idle
				// immediately, before the first runtime push lands.
				rt := m.runtime[rtKey(r.fleetName, instance.Name)]
				label := agentToolLabelProto(rt.GetAgentTool())
				switch rt.GetAgentActivity() {
				case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING:
					agentStr = agentWorkingStyle.Render(fmt.Sprintf("  ▶ %s", label))
					if len(m.agentSpinner.Spinner.Frames) > 0 {
						throbber = strings.TrimRight(m.agentSpinner.View(), "\n")
					} else {
						throbber = agentWorkingStyle.Render("✻")
					}
				case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING:
					agentStr = agentWaitingStyle.Render(fmt.Sprintf("  ⏸ %s", label))
					throbber = agentWaitingStyle.Render("○")
				default:
					agentStr = agentOffStyle.Render("  ○ idle")
				}
			}

			// Automation-spawned instances (issue #188) trail their name with a ⟳
			// marker. It lives INSIDE the fixed-width name column — the name is
			// truncated instanceAutoMarkWidth cells shorter to make room — so the
			// marker never widens the column or shifts the status word, and
			// user-created rows are not indented at all.
			markerSuffix, nameBudget := "", instanceNameWidth
			if instance.Automated {
				markerSuffix = " " + automationMarkStyle.Render(automationMark)
				nameBudget -= instanceAutoMarkWidth
			}

			// Truncate (with an ellipsis) before padding so a long name can't push
			// the status column right and knock the PR-status second line below it
			// out of alignment. Pad by VISUAL width (lipgloss.Width), not fmt's
			// rune count, so a name with wide runes (CJK / emoji) still lands the
			// status at exactly instanceStatusCol.
			name := ansi.Truncate(instance.GetDisplayName(), nameBudget, "…")
			pad := strings.Repeat(" ", max(0, instanceNameWidth-lipgloss.Width(name)-lipgloss.Width(markerSuffix)))
			var arrowStyled, nameStyled string
			switch {
			case isSelected && instanceColorHasCustom(instance.Color):
				colorStyle := instanceColorStyle(instance.Color).Bold(true)
				arrowStyled = colorStyle.Render(arrow)
				nameStyled = colorStyle.Render(name)
			case isSelected:
				arrowStyled = selectedStyle.Render(arrow)
				nameStyled = selectedStyle.Render(name)
			case instanceColorHasCustom(instance.Color):
				colorStyle := instanceColorStyle(instance.Color)
				arrowStyled = colorStyle.Render(arrow)
				nameStyled = colorStyle.Render(name)
			default:
				arrowStyled = arrow
				nameStyled = name
			}
			// name + marker + padding together fill the fixed name column.
			paddedName := arrowStyled + throbber + " " + nameStyled + markerSuffix + pad

			backendIcon := "⬡"
			switch instance.Backend {
			case fleet.BackendCoder:
				backendIcon = "⌨"
			case fleet.BackendCodespaces:
				backendIcon = "⏣"
			}
			branchItem := ""
			if branch := resolveWorkspaceBranch(instance.WorkspaceDir); branch != "" {
				branchItem = dimStyle.Render("  " + branch + " " + backendIcon)
			} else {
				branchItem = dimStyle.Render("  " + backendIcon)
			}

			var line string
			if transitional {
				line = fmt.Sprintf("%s    %s %s%s",
					cursor, paddedName, status, branchItem,
				)
			} else {
				statsStr := ""
				if s := m.runtime[rtKey(r.fleetName, instance.Name)].GetStats(); s != nil {
					statsStr = dimStyle.Render(fmt.Sprintf("  %4.0f mcpu  %6.1f MB", s.GetCpuMillicores(), s.GetMemoryMb()))
				}

				line = fmt.Sprintf("%s    %s %s%s%s%s",
					cursor, paddedName, status, agentStr, statsStr, branchItem,
				)

				pfKey := r.fleetName + "/" + instance.Name
				if pfLabel := m.portForwards.FormatLabels(pfKey); pfLabel != "" {
					line += portForwardStyle.Render("  ⇄ " + pfLabel)
				}
			}

			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}

			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else if r.kind == rowInstanceTag {
			// A user-set tag on its own line under the instance name. (The PR-status
			// auto tag rides the first child row's status column instead — see
			// buildRows and renderChildRowLine.)
			line := fmt.Sprintf("%s        %s", cursor, dimStyle.Render("# "+r.instance.Tag))
			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}
			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else if isAutomationRow(r.kind) {
			line := fleetPage.renderAutomationRow(m, r, cursor, isSelected)
			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}
			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else {
			label := "settings"
			if r.kind == rowLeaveFocus {
				label = "[ leave focus ]"
			}
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, dimStyle.Render(label)))
			}
			listContent.WriteString("\n")
		}
	}

	boxContent := strings.TrimRight(listContent.String(), "\n")
	// The box's own top border is dropped and replaced by a hand-composed
	// line carrying the Armada selector (lipgloss has no border-title API):
	// ╭─ Armada [ local ] ────────╮
	box := listBox.BorderTop(false)
	if m.width > 0 {
		box = box.Width(m.width - 2)
	}
	renderedBox := box.Render(boxContent)
	boxWidth := lipgloss.Width(strings.SplitN(renderedBox, "\n", 2)[0])
	fleetPage.armadaSel.y = strings.Count(b.String(), "\n")
	b.WriteString(fleetPage.renderArmadaBorder(m, boxWidth))
	b.WriteString("\n")
	// Record where rows[0] will land on screen so mouse clicks can map
	// Y → row index. The cursor is at line `newlines` after consuming
	// `b` so far (the armada line above replaced the box's top border);
	// +emptyMsgLines skips the "No instances" line that precedes the
	// (settings-only) rows when no fleets exist.
	emptyMsgLines := 0
	if m.st == nil || len(m.st.Fleets) == 0 {
		emptyMsgLines = 1
	}
	fleetPage.listRowY = strings.Count(b.String(), "\n") + emptyMsgLines
	b.WriteString(renderedBox)
	b.WriteString("\n")

	var totalCPU float64
	var totalMem float64
	statsCount := 0
	for _, rt := range m.runtime {
		if s := rt.GetStats(); s != nil {
			totalCPU += s.GetCpuMillicores()
			totalMem += s.GetMemoryMb()
			statsCount++
		}
	}
	if statsCount > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Total: %.0f mcpu  %.1f MB", totalCPU, totalMem)))
		b.WriteString("\n")
	}

	b.WriteString(fleetPage.viewActiveDialog(m))

	if m.message != "" {
		b.WriteString(messageStyle.Render(m.message))
		b.WriteString("\n")
	}

	// Focus mode hides the help bar entirely (like turning help text off) —
	// it's focus mode, after all.
	showHelp := m.config == nil || m.config.GeneralSettings.ShowHelpTextEnabled()
	if showHelp && fleetPage.focusedFleet == "" {
		b.WriteString(renderHelp(m.width, fleetPage.contextualHelpKeys(m)))
	}

	return b.String()
}

// viewActiveDialog renders the active dialog overlay appended below the
// fleet list. It returns "" when no dialog is open (mode == viewNormal).
func (fleetPage *fleetPage) viewActiveDialog(m *model) string {
	switch fleetPage.mode {
	case viewConfirmDelete:
		return fleetPage.renderConfirmDeleteDialog(m)
	case viewConfirmRebuild:
		return fleetPage.renderConfirmRebuildDialog(m)
	case viewConfirmDeleteFleetWarn:
		return fleetPage.renderConfirmDeleteFleetWarnDialog(m)
	case viewAddInstance:
		return fleetPage.renderAddInstanceDialog(m)
	case viewAddFleet:
		return fleetPage.renderAddFleetDialog(m)
	case viewAddFleetInspecting:
		return fleetPage.renderAddFleetInspectingDialog(m)
	case viewAddFleetNoDevcontainer:
		return fleetPage.renderAddFleetNoDevcontainerDialog(m)
	case viewEditFleet:
		return fleetPage.renderEditFleetDialog(m)
	case viewLayoutPreset:
		return fleetPage.renderLayoutPresetOverlay(m)
	case viewTagInstance:
		return fleetPage.renderTagInstanceDialog(m)
	case viewPortForward:
		return fleetPage.renderPortForwardDialog(m)
	case viewCodespacesAuth:
		return fleetPage.renderCodespacesAuthDialog(m)
	case viewCodespacesMachine:
		return fleetPage.renderCodespacesMachineDialog(m)
	case viewCodespacesLimit:
		return fleetPage.renderCodespacesLimitDialog(m)
	case viewCreateSession:
		return fleetPage.renderCreateSessionDialog(m)
	case viewAutomationTrigger:
		return fleetPage.renderAutomationTriggerDialog(m)
	case viewAutomationAgent:
		return fleetPage.renderAutomationAgentDialog(m)
	case viewCloneInstance:
		return fleetPage.renderCloneInstanceDialog(m)
	case viewRenameSession:
		return fleetPage.renderRenameSessionDialog(m)
	case viewConfirmDeleteSession:
		return fleetPage.renderConfirmDeleteSessionDialog(m)
	case viewConfirmDeleteAutomation:
		return fleetPage.renderConfirmDeleteAutomationDialog(m)
	case viewConfirmBrowserSwitch:
		return fleetPage.renderConfirmBrowserSwitchDialog(m)
	case viewArmadaSelect:
		return fleetPage.renderArmadaSelectDialog(m)
	case viewChooseBrowserLaunch:
		return fleetPage.renderChooseBrowserLaunchDialog(m)
	case viewChoosePR:
		return fleetPage.renderChoosePRDialog(m)
	}
	return ""
}
