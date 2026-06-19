package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	tea "github.com/charmbracelet/bubbletea"
)

// browser.go is the HOST-side half of fleet's browser feature: launching /
// switching / killing the local Chrome process and tracking its data dir. The
// CONTAINER-side work — installing the privoxy proxy, reading the workspace
// devcontainer.json, staging + running the Fleet Launch landing page — lives in
// the server (internal/server/browser.go) and is driven over RPCs
// (GetBrowserConfig / PrepareBrowser) plus the Forward data-plane stream. The
// TUI never touches a backend (the P5 boundary).

// browserProxyPort is the port privoxy listens on inside the container; the host
// forwards a local port to it.
const browserProxyPort = 58888

// ===========================================
// Messages
// ===========================================

// browserProxyMsg is sent when the browser proxy setup completes.
type browserProxyMsg struct {
	instanceKey string
	localPort   int
	err         error
}

// ===========================================
// Commands
// ===========================================

// openBrowserProxyCmd ensures a browser proxy is running for the instance
// (server-side) and launches a Chromium-based browser routed through it.
//
//	Sequence:
//	  1. PrepareBrowser RPC: ensure privoxy is installed/running in the
//	     container and resolve the initial URL (honouring targetURL, else the
//	     devcontainer.json initialUrl / Fleet Launch landing page / default).
//	  2. Reuse or create the host-side port forward (local -> privoxy:58888).
//	  3. Launch the local browser with --proxy-server pointed at the forward.
//
// targetURL, when non-empty, is used verbatim (a control-socket browser.open
// request already chose the URL); the server skips config/landing resolution.
func openBrowserProxyCmd(
	pf *portforward.Manager,
	instanceKey string,
	dataDir string,
	preferFleetLaunch bool,
	targetURL string,
) tea.Cmd {
	fleetName, instanceName, _ := splitInstanceKey(instanceKey)

	return func() tea.Msg {
		// 1. Server: ensure the proxy is up + resolve the page to open.
		initialURL, err := prepareBrowserRemote(fleetName, instanceName, preferFleetLaunch, targetURL)
		if err != nil {
			return browserProxyMsg{instanceKey: instanceKey, err: err}
		}

		// 2. Reuse or create the host-side port forward to privoxy. Each
		// accepted connection is tunnelled over the server's Forward stream
		// (the data plane), so the proxy works even against a remote server.
		localPort, found := pf.FindBrowserProxy(instanceKey)
		if !found {
			dial, err := forwardDialer(fleetName, instanceName, browserProxyPort)
			if err != nil {
				return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("port forward: %w", err)}
			}
			localPort, err = pf.AddBrowserProxy(instanceKey, browserProxyPort, dial)
			if err != nil {
				return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("port forward: %w", err)}
			}
		}

		// 3. Launch the local browser, routed through the proxy.
		if err := launchBrowser(localPort, dataDir, initialURL); err != nil {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("launch browser: %w", err)}
		}
		return browserProxyMsg{instanceKey: instanceKey, localPort: localPort}
	}
}

// beginBrowserOpen is the entry point for the `b` action. When the fleet has
// never had a browser-start preference chosen and the workspace configures BOTH
// an initialUrl and a Fleet Launch landing page, it opens the choose-launch
// dialog (which saves the answer and then launches); otherwise it launches
// straight away. The both-targets check reads the workspace customization via
// the server (GetBrowserConfig).
func (fleetPage *fleetPage) beginBrowserOpen(m *model, instance *fleet.Instance, fleetName string) tea.Cmd {
	if f, ok := m.st.Fleets[fleetName]; ok && !f.Settings.PreferFleetLaunchSet() {
		if initialURL, hasLanding, err := fetchBrowserConfig(fleetName, instance.Name); err == nil && initialURL != "" && hasLanding {
			fleetPage.mode = viewChooseBrowserLaunch
			fleetPage.dlg.fleet = fleetName
			fleetPage.dlg.inst = instance.Name
			fleetPage.dlg.row = chooseBrowserRowFleetLaunch
			return nil
		}
	}
	return fleetPage.startBrowser(m, instance, fleetName)
}

// startBrowser launches (or switches to) the browser for an instance, reading
// the fleet's resolved Fleet-Launch preference. A browser already bound to the
// data dir triggers the switch flow (auto when AutoSwitch is on, otherwise a
// confirm dialog) because Chrome would otherwise forward the launch to the
// existing process and drop our --proxy-server flag.
func (fleetPage *fleetPage) startBrowser(m *model, instance *fleet.Instance, fleetName string) tea.Cmd {
	multiplePerFleet := multipleBrowsersPerFleet(m)
	dataDir := browserDataDir(fleetName, instance.Name, multiplePerFleet)
	instanceKey := fleetName + "/" + instance.Name

	preferFleetLaunch := false
	if f, ok := m.st.Fleets[fleetName]; ok {
		preferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	}

	if _, running := existingBrowserPID(dataDir); running {
		if !multiplePerFleet && m.config != nil && m.config.BrowserSettings.AutoSwitchEnabled() {
			m.message = fmt.Sprintf("Switching browser to %s...", instance.GetDisplayName())
			return switchBrowserCmd(m.portForwards, instanceKey, dataDir, preferFleetLaunch, "")
		}
		fleetPage.mode = viewConfirmBrowserSwitch
		fleetPage.dlg.fleet = fleetName
		fleetPage.dlg.inst = instance.Name
		return nil
	}
	m.message = fmt.Sprintf("Starting browser proxy for %s...", instance.GetDisplayName())
	return openBrowserProxyCmd(m.portForwards, instanceKey, dataDir, preferFleetLaunch, "")
}

// openControlBrowserCmd builds the browser-open command for a control-socket
// "browser.open" request (now delivered as a Watch BrowserOpen event): resolve
// the instance from instanceKey, then open the proxied browser at url.
//
// The subtlety is which instance the existing browser (if any) is proxied to. A
// fleet's instances share one Chrome data dir (unless MultipleBrowsersPerFleet),
// and that Chrome can only route through one instance's privoxy proxy at a time:
//
//   - none running              → start a fresh proxied browser at url.
//   - running, this instance    → forward url as a new tab (the live process
//     already has the right proxy).
//   - running, OTHER instance   → switch: kill it and relaunch against THIS
//     instance, then open url.
//
// activeBrowser records who the live browser serves; an unknown owner is treated
// as "different" and switches (also correct after a host restart: the surviving
// Chrome's old proxy port is dead).
func (m *model) openControlBrowserCmd(instanceKey, url string) tea.Cmd {
	fleetName, instanceName, ok := splitInstanceKey(instanceKey)
	if !ok {
		return func() tea.Msg {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("malformed instance key %q", instanceKey)}
		}
	}

	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return func() tea.Msg {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("fleet %q not found", fleetName)}
		}
	}
	instance, err := f.GetInstance(instanceName)
	if err != nil {
		return func() tea.Msg {
			return browserProxyMsg{instanceKey: instanceKey, err: err}
		}
	}
	if instance.Status != fleet.StatusRunning {
		return func() tea.Msg {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("instance %q is not running", instanceKey)}
		}
	}

	dataDir := browserDataDir(fleetName, instance.Name, multipleBrowsersPerFleet(m))

	// If a browser is already live for this data dir but proxied to a different
	// instance, switch it over (kill + relaunch) instead of letting Chrome
	// forward the URL into the wrong-proxied process.
	_, running := existingBrowserPID(dataDir)
	if shouldSwitchBrowser(running, m.activeBrowser[dataDir], instanceKey) {
		return switchBrowserCmd(m.portForwards, instanceKey, dataDir, false, url)
	}
	return openBrowserProxyCmd(m.portForwards, instanceKey, dataDir, false, url)
}

// shouldSwitchBrowser reports whether a control-socket open for instanceKey must
// switch the live browser (kill + relaunch) rather than reuse it: only when a
// browser is actually running for the data dir AND it is not already proxied to
// instanceKey (an unknown/empty owner counts as "not this instance").
func shouldSwitchBrowser(running bool, activeInstanceKey, instanceKey string) bool {
	return running && activeInstanceKey != instanceKey
}

// switchBrowserCmd kills any browser currently bound to dataDir and then runs
// the normal open-browser-proxy flow against the given instance. The two steps
// are folded into one tea.Cmd so the UI can keep a "Switching..." spinner up for
// the whole kill+relaunch window.
func switchBrowserCmd(
	pf *portforward.Manager,
	instanceKey string,
	dataDir string,
	preferFleetLaunch bool,
	targetURL string,
) tea.Cmd {
	inner := openBrowserProxyCmd(pf, instanceKey, dataDir, preferFleetLaunch, targetURL)
	return func() tea.Msg {
		if err := killExistingBrowser(dataDir); err != nil {
			return browserProxyMsg{
				instanceKey: instanceKey,
				err:         fmt.Errorf("stop existing browser: %w", err),
			}
		}
		return inner()
	}
}

// ===========================================
// Host-side browser process
// ===========================================

// launchBrowser opens a Chromium-based browser with its HTTP/HTTPS/WS/WSS
// traffic routed through the proxy on localPort. The user data directory is
// supplied by the caller so the layout (per-fleet vs per-instance) is owned by
// configuration. initialURL is the page resolved by the server.
func launchBrowser(localPort int, dataDir, initialURL string) error {
	proxyArg := fmt.Sprintf("--proxy-server=http://localhost:%d", localPort)

	// Chrome won't reliably start if the data dir doesn't exist on first launch;
	// create it eagerly with the same permissions used elsewhere in fleet-man.
	if err := os.MkdirAll(dataDir, 0777); err != nil {
		return fmt.Errorf("create browser data dir: %w", err)
	}

	browsers := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium-browser",
		"chromium",
	}

	for _, browser := range browsers {
		if _, err := exec.LookPath(browser); err == nil {
			cmd := exec.Command(browser,
				proxyArg,
				// By default Chrome bypasses the proxy for loopback addresses;
				// the whole point is to reach the container's localhost, so
				// disable that bypass.
				"--proxy-bypass-list=<-loopback>",
				"--user-data-dir="+dataDir,
				"--no-first-run",
				"--no-default-browser-check",
				initialURL,
			)
			return cmd.Start()
		}
	}

	return fmt.Errorf("no Chromium-based browser found (tried: %s)", strings.Join(browsers, ", "))
}

// ===========================================
// Data dir & singleton detection
// ===========================================

// browserDataDirName is the subdirectory under a fleet (or instance) where
// Chrome's user-data-dir lives.
const browserDataDirName = ".browser"

// browserDataDir returns the host path Chrome should use as its --user-data-dir.
//
//	false (default): ~/.fleet/workspaces/<fleet>/.browser
//	true:            ~/.fleet/workspaces/<fleet>/<instance>/.browser
func browserDataDir(fleetName, instanceName string, multiplePerFleet bool) string {
	if multiplePerFleet {
		return filepath.Join(fleetpaths.WorkspacesDir(), fleetName, instanceName, browserDataDirName)
	}
	return filepath.Join(fleetpaths.WorkspacesDir(), fleetName, browserDataDirName)
}

// existingBrowserPID returns the PID of a Chrome process that currently owns
// dataDir, if one is running. It reads Chrome's SingletonLock symlink (target
// "<hostname>-<pid>"). A stale lock (process gone) returns ok=false.
func existingBrowserPID(dataDir string) (int, bool) {
	target, err := os.Readlink(filepath.Join(dataDir, "SingletonLock"))
	if err != nil {
		return 0, false
	}
	idx := strings.LastIndex(target, "-")
	if idx < 0 || idx == len(target)-1 {
		return 0, false
	}
	pid, err := strconv.Atoi(target[idx+1:])
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

// killExistingBrowser terminates the Chrome process that owns dataDir and cleans
// up the singleton metadata files so the next launch is not rejected. Returns
// nil if no browser was running or after a successful shutdown.
func killExistingBrowser(dataDir string) error {
	pid, ok := existingBrowserPID(dataDir)
	if !ok {
		removeSingletonFiles(dataDir)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find browser process %d: %w", pid, err)
	}
	// SIGTERM lets Chrome shut down cleanly; escalate to SIGKILL if it doesn't
	// exit within a short window.
	_ = proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
		time.Sleep(200 * time.Millisecond)
	}

	removeSingletonFiles(dataDir)
	return nil
}

// removeSingletonFiles clears Chrome's three singleton-tracking symlinks from
// dataDir. Safe to call when they don't exist.
func removeSingletonFiles(dataDir string) {
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}

// multipleBrowsersPerFleet returns the user's preference for whether each
// instance gets its own browser data dir. Falls back to false when no config is
// loaded yet so first-launch behavior matches the documented default.
func multipleBrowsersPerFleet(m *model) bool {
	if m.config == nil {
		return false
	}
	return m.config.BrowserSettings.MultipleBrowsersPerFleetEnabled()
}

// updateConfirmBrowserSwitch handles the prompt shown when the user asks
// to open a browser for an instance but another browser is already
// running against the same user-data-dir. Default action is "yes,
// switch": Chrome's singleton lock means the existing process must be
// killed before a fresh launch with this instance's proxy can succeed.
//
// Once the user confirms, the dialog stays open but its body swaps to a
// "Switching..." spinner — the kill+relaunch cmd runs asynchronously and
// the dialog is torn down when browserProxyMsg returns.
func (fleetPage *fleetPage) updateConfirmBrowserSwitch(m *model, msg tea.Msg) tea.Cmd {
	// While the kill+relaunch is in flight, swallow input so the user
	// can't double-trigger the flow.
	if fleetPage.browserDlg.switching {
		return nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = "Cancelled"
			return nil

		// Anything else (y/Y/enter/space) confirms — default-yes
		// matches the issue spec, so we treat unrecognised keys as
		// "proceed" rather than swallowing them.
		default:
			fleetName := fleetPage.dlg.fleet
			instanceName := fleetPage.dlg.inst

			f, ok := m.st.Fleets[fleetName]
			if !ok {
				fleetPage.mode = viewNormal
				m.message = "Fleet no longer exists"
				return nil
			}
			instance, err := f.GetInstance(instanceName)
			if err != nil {
				fleetPage.mode = viewNormal
				m.message = fmt.Sprintf("Instance no longer exists: %v", err)
				return nil
			}
			if instance.Status != fleet.StatusRunning {
				fleetPage.mode = viewNormal
				m.message = "Instance must be running to open browser"
				return nil
			}

			dataDir := browserDataDir(fleetName, instanceName, multipleBrowsersPerFleet(m))
			instanceKey := fleetName + "/" + instanceName

			// Switch the dialog into "in-flight" mode; the renderer
			// will draw the spinner. The cmd does the kill + relaunch
			// in the background and the resulting browserProxyMsg
			// clears the flag and the mode.
			fleetPage.browserDlg.switching = true
			m.message = ""
			return switchBrowserCmd(m.portForwards, instanceKey, dataDir, f.Settings.PreferFleetLaunchEnabled(), "")
		}
	}
	return nil
}

// chooseBrowserRow identifies a selectable option in the
// choose-browser-launch dialog.
const (
	chooseBrowserRowFleetLaunch = iota
	chooseBrowserRowInitialURL
	chooseBrowserRowCount
)

// updateChooseBrowserLaunch handles the dialog shown the first time the
// browser is opened on a fleet whose workspace configures both an
// initialUrl and a Fleet Launch landing page. Navigation matches the rest
// of fleet — j/k or arrows move the cursor, enter/space chooses the
// selected row — and the [f]/[u] shortcuts jump straight to a choice. The
// answer is saved as the fleet's PreferFleetLaunch setting and the browser
// then launches.
func (fleetPage *fleetPage) updateChooseBrowserLaunch(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		m.message = "Cancelled"
		return nil
	case "up", "k":
		fleetPage.dlg.row = (fleetPage.dlg.row - 1 + chooseBrowserRowCount) % chooseBrowserRowCount
		return nil
	case "down", "j", "tab":
		fleetPage.dlg.row = (fleetPage.dlg.row + 1) % chooseBrowserRowCount
		return nil
	case "enter", " ":
		return fleetPage.chooseBrowserLaunch(m, fleetPage.dlg.row == chooseBrowserRowFleetLaunch)
	case "f", "F":
		return fleetPage.chooseBrowserLaunch(m, true)
	case "u", "U":
		return fleetPage.chooseBrowserLaunch(m, false)
	}
	return nil
}

// chooseBrowserLaunch persists the browser-start preference for the fleet
// and proceeds with the launch using it.
func (fleetPage *fleetPage) chooseBrowserLaunch(m *model, preferFleetLaunch bool) tea.Cmd {
	fleetName := fleetPage.dlg.fleet
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		fleetPage.mode = viewNormal
		m.message = "Fleet no longer exists"
		return nil
	}

	prefer := preferFleetLaunch
	f.Settings.PreferFleetLaunch = &prefer
	_ = setFleetSettingsRemote(fleetName, f.Settings)

	instance, err := f.GetInstance(fleetPage.dlg.inst)
	if err != nil || instance.Status != fleet.StatusRunning {
		fleetPage.mode = viewNormal
		m.message = "Instance must be running to open browser"
		return nil
	}

	fleetPage.mode = viewNormal
	return fleetPage.startBrowser(m, instance, fleetName)
}

// browserDialogState holds the switch-browser dialog's in-flight flag: true
// while the dialog has dispatched a kill+relaunch but the browserProxyMsg has
// not yet returned, so the body can swap to a spinner during the SIGTERM
// grace period.
type browserDialogState struct {
	switching bool
}

func (fleetPage *fleetPage) renderConfirmBrowserSwitchDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	var dialog string
	if fleetPage.browserDlg.switching {
		dialog = fmt.Sprintf(
			"%s\n\n%s %s",
			dialogTitle.Render("Switch browser"),
			m.spinner.View(),
			dialogLabel.Render(fmt.Sprintf(
				"Switching browser to %s/%s...",
				fleetPage.dlg.fleet, fleetPage.dlg.inst,
			)),
		)
	} else {
		dialog = fmt.Sprintf(
			"%s\n\n%s\n\n%s\n\n%s",
			dialogTitle.Render("Switch browser"),
			dialogLabel.Render(fmt.Sprintf(
				"Another browser is running. Switch it to %s/%s?",
				fleetPage.dlg.fleet, fleetPage.dlg.inst,
			)),
			dimStyle.Render("For two at once: Settings → Browser → Multiple Browsers (separate profiles per instance)."),
			dialogHint.Render("[Y/enter] Yes (default)  [n/q/esc] No"),
		)
	}
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderChooseBrowserLaunchDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	// Cursor-selectable option line: "> " marker + pink highlight on
	// the focused row, matching the fleet list's selection style.
	optLine := func(r int, label string) string {
		if fleetPage.dlg.row == r {
			return cursorStyle.Render("> ") + selectedStyle.Render(label)
		}
		return "  " + dialogLabel.Render(label)
	}
	dialog := fmt.Sprintf(
		"%s\n\n%s\n\n%s\n%s\n\n%s\n\n%s",
		dialogTitle.Render("Choose browser start page"),
		dialogLabel.Render("This project configures both a Fleet Launch landing page and an initialUrl. Which should the browser open?"),
		optLine(chooseBrowserRowFleetLaunch, "[f] Fleet Launch"),
		optLine(chooseBrowserRowInitialURL, "[u] Initial URL"),
		dimStyle.Render("Selection will be saved to fleet settings"),
		dialogHint.Render("[j/k] Select  [enter/space] Choose  [f/u] Shortcut  [q/esc] Cancel"),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")
	return b.String()
}
