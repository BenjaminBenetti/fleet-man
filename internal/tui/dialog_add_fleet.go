package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/devcontainersetup"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	devcontainercheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/devcontainer"
	tea "github.com/charmbracelet/bubbletea"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// updateAddFleet handles the add-fleet dialog.
//
// Pressing enter does not immediately persist the fleet — instead, it
// kicks off an asynchronous inspection (clone + .devcontainer lookup)
// and switches to viewAddFleetInspecting so the user sees a spinner
// while the network work runs. The inspect result is delivered via
// devcontainerInspectedMsg and resumed in handleDevcontainerInspected.
func (fleetPage *fleetPage) updateAddFleet(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveAddFleet(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

// saveAddFleet validates the URL and kicks off the asynchronous
// devcontainer inspection. The fleet is NOT persisted here — that
// happens later in handleDevcontainerInspected (devcontainer present)
// or in updateAddFleetNoDevcontainer's Setup branch (devcontainer
// missing but user opted into the agent flow). Aborting either dialog
// after this point therefore leaves no trace in state.
//
// A file:// template URL detours through the name prompt first
// (viewAddFleetName): a local directory carries no repo name to derive the
// fleet name from, so the user has to supply one before inspection runs.
func (fleetPage *fleetPage) saveAddFleet(m *model) tea.Cmd {
	repoURL := strings.TrimSpace(fleetPage.textInput.Value())
	if repoURL == "" {
		m.message = "URL cannot be empty"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	if fleet.IsTemplateRemote(repoURL) {
		return fleetPage.promptAddFleetName(m, repoURL)
	}
	fleetName := fleet.FleetNameFromRemote(repoURL)
	if fleetName == "" {
		m.message = "Could not derive fleet name from URL"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	fleetPage.addFleet.pendingRepoURL = repoURL
	fleetPage.addFleet.pendingFleetName = fleetName
	fleetPage.mode = viewAddFleetInspecting
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Inspecting %s...", repoURL)
	return inspectDevcontainerCmd(fleetName, repoURL)
}

// ===========================================
// Template Fleet Name Prompt
// ===========================================

// promptAddFleetName validates the template URL's shape (an absolute path is
// the only thing the client can check — the directory itself lives on the
// daemon host and is verified by the inspection) and switches to the
// fleet-name prompt, pre-filled with the directory's base name as a
// suggestion the user can accept or overwrite.
func (fleetPage *fleetPage) promptAddFleetName(m *model, repoURL string) tea.Cmd {
	if _, err := fleet.TemplateDir(repoURL); err != nil {
		// Keep the dialog open so the user can fix the URL in place.
		m.message = err.Error()
		return nil
	}
	fleetPage.addFleet.pendingRepoURL = repoURL
	fleetPage.mode = viewAddFleetName
	fleetPage.textInput.SetValue(fleet.TemplateNameHint(repoURL))
	fleetPage.textInput.Placeholder = "fleet-name"
	fleetPage.textInput.CharLimit = 64
	fleetPage.textInput.CursorEnd()
	m.message = ""
	return fleetPage.activateTextInput()
}

// updateAddFleetName handles the fleet-name prompt shown for a file://
// template URL. Inside the field: enter submits, esc leaves the field,
// ctrl+c cancels. With the field inactive: esc goes BACK to the URL step (so
// a typo in the path can be fixed without starting over), q/ctrl+c cancel.
func (fleetPage *fleetPage) updateAddFleetName(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveAddFleetName(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				fleetPage.clearPendingFleet()
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter", " ":
			return fleetPage.activateTextInput()
		case "esc":
			return fleetPage.returnToAddFleetURL(m)
		case "q", "Q", "ctrl+c":
			fleetPage.clearPendingFleet()
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}
	return nil
}

// returnToAddFleetURL reopens the URL step with the pending URL back in the
// input. The name prompt borrows textInput for the fleet name, so both the
// esc-back key and the inspection error path need this restore.
func (fleetPage *fleetPage) returnToAddFleetURL(m *model) tea.Cmd {
	fleetPage.mode = viewAddFleet
	fleetPage.textInput.SetValue(fleetPage.addFleet.pendingRepoURL)
	fleetPage.textInput.Placeholder = addFleetURLPlaceholder
	fleetPage.textInput.CharLimit = 256
	fleetPage.textInput.CursorEnd()
	fleetPage.textInput.Focus()
	return fleetPage.textInput.Cursor.BlinkCmd()
}

// saveAddFleetName validates the typed fleet name and continues into the
// same inspection the git path runs (against the template dir, on the daemon
// host). An existing fleet name is refused rather than silently reused:
// CreateFleet is get-or-create, so "adding" it would keep the OTHER fleet's
// remote and the user's template would never be copied.
func (fleetPage *fleetPage) saveAddFleetName(m *model) tea.Cmd {
	fleetName := strings.TrimSpace(fleetPage.textInput.Value())
	if err := fleet.ValidateFleetName(fleetName); err != nil {
		m.message = err.Error()
		return nil
	}
	if _, exists := m.st.Fleets[fleetName]; exists {
		m.message = fmt.Sprintf("Fleet %s already exists", fleetName)
		return nil
	}
	repoURL := fleetPage.addFleet.pendingRepoURL
	fleetPage.addFleet.pendingFleetName = fleetName
	fleetPage.mode = viewAddFleetInspecting
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Inspecting %s...", repoURL)
	return inspectDevcontainerCmd(fleetName, repoURL)
}

// ===========================================
// Devcontainer Inspection (new fleet)
// ===========================================

// devcontainerInspectedMsg is delivered when the asynchronous repo
// clone + devcontainer.json lookup completes. The fleetName is echoed
// back so a stale result (the user dismissed the dialog before the
// clone finished) can be discarded.
type devcontainerInspectedMsg struct {
	fleetName       string
	hasDevcontainer bool
	err             error
}

// inspectDevcontainerCmd asks the SERVER to clone the repo and check for a
// devcontainer config, in a background goroutine. Inspection runs on the
// daemon's host — the machine that will actually clone at provision time — so
// a remote TUI gets the verdict of the daemon's git credentials, not its own
// (issue #141 note 5).
//
// A clone failure surfaces with err set so the caller can report it
// rather than blindly assuming the repo lacks a devcontainer — an
// unreachable URL is a different problem than a configured-but-missing
// devcontainer, and the user almost certainly wants to fix the URL
// before being offered a setup workflow.
func inspectDevcontainerCmd(fleetName, remoteURL string) tea.Cmd {
	return func() tea.Msg {
		reply, err := inspectRepoRemote(remoteURL, "", false)
		if grpcstatus.Code(err) == grpccodes.Unimplemented {
			// Compatibility fallback for daemons that predate InspectRepo:
			// clone + check locally like the TUI always used to.
			return inspectDevcontainerLocal(fleetName, remoteURL)
		}
		if err != nil {
			// Unwrap the status so the user sees the clone error itself, not
			// the "rpc error: code = ..." framing around it.
			return devcontainerInspectedMsg{fleetName: fleetName, err: errors.New(grpcstatus.Convert(err).Message())}
		}
		return devcontainerInspectedMsg{
			fleetName:       fleetName,
			hasDevcontainer: reply.GetHasDevcontainer(),
		}
	}
}

// inspectDevcontainerLocal is the pre-InspectRepo behavior — a shallow clone
// with THIS process's credentials — kept only as the compatibility fallback
// above. The Repo handle is closed before the message is returned so the temp
// clone never outlives the command.
func inspectDevcontainerLocal(fleetName, remoteURL string) tea.Msg {
	repo, err := inspector.Open(remoteURL, "")
	if err != nil {
		return devcontainerInspectedMsg{fleetName: fleetName, err: err}
	}
	defer repo.Close()
	present, err := devcontainercheck.Present(repo)
	return devcontainerInspectedMsg{
		fleetName:       fleetName,
		hasDevcontainer: present,
		err:             err,
	}
}

// handleDevcontainerInspected resumes the new-fleet flow once the
// asynchronous inspection has completed. There are three branches:
//
//  1. clone failed → surface the error, drop back to the URL input so
//     the user can correct it. The fleet is not persisted.
//  2. devcontainer present → persist the fleet immediately and dismiss.
//  3. devcontainer missing → switch to the no-devcontainer dialog so
//     the user can choose to abort or launch the setup agent.
//
// Stale results from a dialog the user has already abandoned are
// dropped silently.
func (fleetPage *fleetPage) handleDevcontainerInspected(m *model, msg devcontainerInspectedMsg) tea.Cmd {
	if fleetPage.mode != viewAddFleetInspecting || fleetPage.addFleet.pendingFleetName != msg.fleetName {
		return nil
	}

	if msg.err != nil {
		// Back to the URL step (with the URL restored — the template name
		// prompt reuses textInput) so the user can correct it in place.
		m.message = fmt.Sprintf("Could not inspect repo: %v", msg.err)
		return fleetPage.returnToAddFleetURL(m)
	}

	if msg.hasDevcontainer {
		fleetPage.addPendingFleet(m)
		m.message = fmt.Sprintf("Added fleet %s", fleetPage.addFleet.pendingFleetName)
		fleetPage.clearPendingFleet()
		fleetPage.mode = viewNormal
		return nil
	}

	fleetPage.mode = viewAddFleetNoDevcontainer
	return nil
}

// addPendingFleet creates the fleet record for whichever URL is
// currently pending and rebuilds the row list. Used by both the
// "devcontainer present → just add it" success path and the
// "user picked Setup → optimistically add then hand off to agent"
// branch.
func (fleetPage *fleetPage) addPendingFleet(m *model) {
	m.st.GetOrCreateFleet(fleetPage.addFleet.pendingFleetName, fleetPage.addFleet.pendingRepoURL)
	_ = createFleetRemote(fleetPage.addFleet.pendingFleetName, fleetPage.addFleet.pendingRepoURL)
	fleetPage.buildRows(m)
}

// clearPendingFleet wipes the per-dialog scratch fields once the
// inspect/setup workflow finishes (success, abort, or error). The
// values are not load-bearing after the dialog closes; resetting them
// keeps a future open-this-dialog-again from seeing stale data.
func (fleetPage *fleetPage) clearPendingFleet() {
	fleetPage.addFleet.pendingRepoURL = ""
	fleetPage.addFleet.pendingFleetName = ""
}

// ===========================================
// No-Devcontainer Dialog
// ===========================================

// updateAddFleetInspecting handles input while the
// "Inspecting <repo>..." spinner is on screen. The user can press esc /
// ctrl+c to bail out of the new-fleet flow without waiting for the
// clone to finish (the goroutine will still complete and the result
// will be dropped by the stale-mode guard in
// handleDevcontainerInspected).
func (fleetPage *fleetPage) updateAddFleetInspecting(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		fleetPage.clearPendingFleet()
		m.message = "Cancelled"
	}
	return nil
}

// updateAddFleetNoDevcontainer handles the dialog shown when the
// inspected repo has no devcontainer.json. Two paths:
//
//   - Abort (default; esc / n / a / enter): drop the pending fleet
//     and return to the fleet list without persisting anything.
//   - Setup (s): persist the fleet optimistically (so the user can see
//     it in the list while they work) and hand off to the local
//     coding agent with a devcontainer-authoring prompt. The agent's
//     stdio takes over the terminal; when it exits (ctrl+c / ctrl+d)
//     bubbletea repaints and we are back in the fleet list.
func (fleetPage *fleetPage) updateAddFleetNoDevcontainer(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "s", "S":
		repoURL := fleetPage.addFleet.pendingRepoURL
		fleetName := fleetPage.addFleet.pendingFleetName

		cmd, err := devcontainersetup.Command(repoURL)
		if err != nil {
			fleetPage.mode = viewNormal
			fleetPage.clearPendingFleet()
			m.message = fmt.Sprintf("No coding agent available: %v", err)
			return nil
		}

		// Add the fleet immediately, before launching the agent. The
		// issue spec is explicit: assume the user follows through. If
		// they bail mid-setup the fleet still appears in the list so
		// they can return to it (or delete it) later.
		fleetPage.addPendingFleet(m)
		m.message = fmt.Sprintf("Added fleet %s — launching setup agent...", fleetName)
		fleetPage.clearPendingFleet()
		fleetPage.mode = viewNormal

		return execProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })

	case "a", "A", "n", "N", "q", "Q", "esc", "ctrl+c", "enter":
		fleetPage.mode = viewNormal
		fleetPage.clearPendingFleet()
		m.message = "Cancelled — fleet not added"
		return nil
	}
	return nil
}

// ===========================================
// Edit Fleet Dialog
// ===========================================

// addFleetState holds the new-fleet flow's pending fields, carried across the
// asynchronous devcontainer inspection so the inspect-result handler can fall
// through into either adding the fleet or showing the no-devcontainer prompt
// without re-asking the user.
type addFleetState struct {
	pendingRepoURL   string
	pendingFleetName string
}

// addFleetURLPlaceholder is the New fleet dialog's URL placeholder; the
// inspection error path re-applies it after the template name prompt has
// borrowed the input.
const addFleetURLPlaceholder = "git@github.com:org/repo.git"

// addFleetSourceLabel names the pending source in the inspect / no-devcontainer
// dialogs: a template dir is copied, not cloned, so calling it "Repo" would
// misdescribe what fleet is about to do with it.
func (fleetPage *fleetPage) addFleetSourceLabel() string {
	if fleet.IsTemplateRemote(fleetPage.addFleet.pendingRepoURL) {
		return "Template:"
	}
	return "Repo:    "
}

func (fleetPage *fleetPage) renderAddFleetDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n\n%s\n%s",
		dialogTitle.Render("New fleet"),
		dialogLabel.Render("Repo:"),
		fleetPage.textInput.View(),
		dimStyle.Render("git URL to clone, or file:///abs/dir to copy a local template dir into each instance"),
		dialogHint.Render(fleetPage.textDialogHint("Add")),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

// addFleetNameHint is textDialogHint plus the name prompt's extra key: esc
// with the field inactive returns to the URL step instead of cancelling.
func (fleetPage *fleetPage) addFleetNameHint() string {
	if fleetPage.dlg.fieldActive {
		return fleetPage.textDialogHint("Add")
	}
	return "[enter] Edit  [esc] Back  [q] Cancel"
}

func (fleetPage *fleetPage) renderAddFleetNameDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n%s %s\n\n%s\n%s",
		dialogTitle.Render("New fleet"),
		dialogLabel.Render("Template:"),
		fleetExpandedStyle.Render(fleetPage.addFleet.pendingRepoURL),
		dialogLabel.Render("Name:    "),
		fleetPage.textInput.View(),
		dimStyle.Render("a local dir has no repo name to derive the fleet name from — pick one"),
		dialogHint.Render(fleetPage.addFleetNameHint()),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderAddFleetInspectingDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n\n%s %s\n\n%s",
		dialogTitle.Render("New fleet"),
		dialogLabel.Render(fleetPage.addFleetSourceLabel()),
		fleetExpandedStyle.Render(fleetPage.addFleet.pendingRepoURL),
		m.spinner.View(),
		dialogLabel.Render("Inspecting for devcontainer.json..."),
		dialogHint.Render("[q/esc] Cancel"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderAddFleetNoDevcontainerDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	agentName, _, agentErr := devcontainersetup.FindAgent()
	var setupLine string
	if agentErr != nil {
		setupLine = statusCreatingStyle.Render("no agent found") +
			"  " + dimStyle.Render("install claude, codex, gemini, or copilot to use Setup")
	} else {
		setupLine = statusRunningStyle.Render(agentName) +
			"  " + dimStyle.Render("will clone the repo and walk you through configuration")
	}
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s %s\n\n%s\n\n%s\n\n%s",
		warnBanner.Render("  No devcontainer.json found  "),
		dialogLabel.Render(
			"This repository has no .devcontainer/devcontainer.json.\n"+
				"fleet-man needs one before it can provision instances.",
		),
		dialogLabel.Render(fleetPage.addFleetSourceLabel()),
		fleetExpandedStyle.Render(fleetPage.addFleet.pendingRepoURL),
		dialogLabel.Render("Setup agent: ")+setupLine,
		dialogLabel.Render(
			"[a] Abort — do not add the fleet (default)\n"+
				"[s] Setup — add the fleet now and launch a guided agent to write the devcontainer",
		),
		dialogHint.Render("[a/q/enter/esc] Abort  [s] Setup"),
	)
	b.WriteString(warnBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}
