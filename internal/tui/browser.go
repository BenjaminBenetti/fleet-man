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

	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/landingpage"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Constants
// ===========================================

// browserProxyPort is the port privoxy listens on inside the container.
const browserProxyPort = 58888

// defaultBrowserURL is the page the browser opens to when a fleet
// customization in the workspace's devcontainer.json does not specify
// an initialUrl (see customizations.fleet.browser.initialUrl).
const defaultBrowserURL = "about:blank"

// landingPagePidPath and landingPageLogPath track the in-container
// landing-page server fleet-launch hosts. They are namespaced under
// fleet-launch so future in-container services (each fronted by its
// own subcommand of the staged binary) can claim their own /tmp paths
// without colliding. The binary itself lives at fleetlaunch.RemotePath.
const (
	landingPagePidPath = "/tmp/fleet-launch-landingpage.pid"
	landingPageLogPath = "/tmp/fleet-launch-landingpage.log"
)

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
// Proxy Setup Script
// ===========================================

// privoxy is used instead of tinyproxy because tinyproxy mishandles
// responses with multiple Set-Cookie headers (common in older auth
// flows that split a large cookie across several lines). privoxy
// forwards them verbatim.

// privoxyEnsureInstalled installs privoxy if it is not already
// present. Mirrors the pattern used by tmuxEnsureInstalled in
// dotfiles.go: try without sudo first, then fall back to sudo,
// across multiple package managers.
var privoxyEnsureInstalled = `command -v privoxy >/dev/null 2>&1 || { echo '==> Installing privoxy...'; (apt-get update -qq && apt-get install -y -qq privoxy) 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq privoxy) 2>/dev/null || (apk add privoxy) 2>/dev/null || (sudo apk add privoxy) 2>/dev/null || (dnf install -y privoxy) 2>/dev/null || (sudo dnf install -y privoxy) 2>/dev/null || (yum install -y privoxy) 2>/dev/null || (sudo yum install -y privoxy) 2>/dev/null || { echo 'INSTALL_FAILED'; exit 1; }; }; `

// privoxyAction neutralizes privoxy's built-in privacy/filtering
// behaviors. fleet-man only wants transparent forwarding to dev
// services inside the container — no ad-blocking, no header
// rewriting, no cookie crunching. limit-connect is widened to the
// full port range so HTTPS CONNECT works for services on any port
// (privoxy's default only permits CONNECT to 443).
var privoxyAction = `{ -block -filter -handle-as-image -set-image-blocker -hide-from-header -hide-referrer -hide-user-agent -crunch-incoming-cookies -crunch-outgoing-cookies -session-cookies-only -change-x-forwarded-for +limit-connect{1-65535} }\n/\n`

// privoxyConfig is a minimal privoxy configuration. Allows connections
// from any source (traffic arrives via the port forward, not the open
// network) and loads only fleet-man's own action file so system-wide
// privoxy rules don't leak in. confdir points at the package-installed
// template directory so privoxy can render its own error pages.
var privoxyConfig = `listen-address 0.0.0.0:58888\npermit-access 0.0.0.0/0\ntoggle 0\nenable-remote-toggle 0\nenable-remote-http-toggle 0\nenable-edit-actions 0\nconfdir /etc/privoxy\nactionsfile /tmp/fleet-proxy.action\n`

// proxySetupScript is the single-line shell command that installs
// privoxy (if missing) and starts it idempotently.
//
//	Flow:
//	  1. Install privoxy via the first available package manager
//	     (with sudo fallback).
//	  2. If a privoxy process is already running, exit early.
//	  3. Write action + config files to /tmp/ via printf.
//	  4. Start privoxy as a daemon with an explicit pidfile.
var proxySetupScript = privoxyEnsureInstalled +
	`if [ -f /tmp/fleet-proxy.pid ] && kill -0 $(cat /tmp/fleet-proxy.pid) 2>/dev/null; then echo 'ALREADY_RUNNING'; exit 0; fi; ` +
	`printf '` + privoxyAction + `' > /tmp/fleet-proxy.action; ` +
	`printf '` + privoxyConfig + `' > /tmp/fleet-proxy.conf; ` +
	`privoxy --pidfile /tmp/fleet-proxy.pid /tmp/fleet-proxy.conf && echo 'STARTED' || { echo 'START_FAILED'; exit 1; }`

// ===========================================
// Commands
// ===========================================

// openBrowserProxyCmd returns a tea.Cmd that ensures a browser proxy is
// running for the given instance and then launches a Chromium-based
// browser configured to route traffic through it.
//
//	Sequence:
//	  +-----------------------+
//	  | Existing proxy fwd?   |--yes--> Launch browser
//	  +-----------------------+
//	            |no
//	  +-----------------------+
//	  | Create port forward   |
//	  | (auto local port)     |
//	  +-----------------------+
//	            |
//	  +-----------------------+
//	  | Install / start       |
//	  | privoxy in container  |
//	  +-----------------------+
//	            |
//	  +-----------------------+
//	  | Launch browser with   |
//	  | --proxy-server flag   |
//	  +-----------------------+
//
// targetURL, when non-empty, overrides the page the browser opens to: it is
// used verbatim as the initial URL and the config-resolution / landing-page
// startup block is skipped entirely. This is the path taken by control-socket
// "browser.open" requests, where the in-instance TUI has already chosen the
// exact URL (a site link, or http://localhost:<port> for an app). An empty
// targetURL preserves the original behaviour: resolve the page from the
// workspace's devcontainer.json fleet customization.
func openBrowserProxyCmd(
	pf *portforward.Manager,
	instanceBackend backend.Backend,
	instance *fleet.Instance,
	instanceKey string,
	dataDir string,
	preferFleetLaunch bool,
	targetURL string,
) tea.Cmd {
	// Capture values for the goroutine.
	workspaceDir := instance.WorkspaceDir
	containerID := instance.ContainerID

	return func() tea.Msg {
		// 1. Reuse an existing browser proxy forward if one exists.
		localPort, found := pf.FindBrowserProxy(instanceKey)

		if !found {
			// 2. Create a new port forward from an auto-selected local
			//    port to privoxy inside the container.
			var err error
			localPort, err = pf.AddBrowserProxy(
				instanceKey,
				browserProxyPort,
				instanceBackend.PortForwardCommand,
				containerID,
				instanceBackend.ResolveHostname,
			)
			if err != nil {
				return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("port forward: %w", err)}
			}
		}

		// 3. Ensure privoxy is installed and running inside the container.
		if err := ensureProxyRunning(instanceBackend, workspaceDir); err != nil {
			return browserProxyMsg{instanceKey: instanceKey, err: err}
		}

		// 4. Resolve the page the browser opens to from the workspace's
		//    devcontainer.json fleet customization. Two pages can be
		//    configured — browser.initialUrl and the fleetLaunch block
		//    (sites/apps) — and precedence between them is:
		//      - only one configured: use that one.
		//      - both configured: the fleet's PreferFleetLaunch setting
		//        decides — false (default) opens initialUrl, true opens
		//        the Fleet Launch landing page.
		//      - neither: the default page.
		//    Choosing the landing page injects and starts fleet-man's own
		//    binary as the landing-page server in the container. A missing
		//    or malformed config (load error) falls back to the default
		//    rather than blocking the launch.
		//
		//    When the caller supplied an explicit targetURL (a control-socket
		//    "browser.open" request), skip all of this: the URL was already
		//    chosen by the in-instance TUI, so there is no config to resolve
		//    and no landing page to start.
		initialURL := defaultBrowserURL
		if targetURL != "" {
			initialURL = targetURL
		} else if fc, err := devcontainer.LoadFleetCustomizations(workspaceDir); err == nil {
			hasURL := fc.Browser.InitialURL != ""
			hasLanding := fc.FleetLaunch.Configured()
			useLanding := shouldUseLandingPage(hasURL, hasLanding, preferFleetLaunch)

			switch {
			case useLanding:
				if err := ensureLandingPageRunning(instanceBackend, workspaceDir); err != nil {
					return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("landing page: %w", err)}
				}
				initialURL = appstart.LocalURL(landingpage.DefaultPort)
			case hasURL:
				initialURL = fc.Browser.InitialURL
			}
		}
		if err := launchBrowser(localPort, dataDir, initialURL); err != nil {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("launch browser: %w", err)}
		}

		return browserProxyMsg{instanceKey: instanceKey, localPort: localPort}
	}
}

// beginBrowserOpen is the entry point for the `b` action. When the fleet
// has never had a browser-start preference chosen and the workspace
// configures BOTH an initialUrl and a Fleet Launch landing page, it opens
// the choose-launch dialog (which saves the answer and then launches);
// otherwise it launches straight away.
func (fleetPage *fleetPage) beginBrowserOpen(m *model, instance *fleet.Instance, fleetName string) tea.Cmd {
	if f, ok := m.st.Fleets[fleetName]; ok && !f.Settings.PreferFleetLaunchSet() {
		if _, both := bothBrowserTargets(instance.WorkspaceDir); both {
			fleetPage.mode = viewChooseBrowserLaunch
			fleetPage.dialogFleet = fleetName
			fleetPage.dialogInst = instance.Name
			fleetPage.dialogRow = chooseBrowserRowFleetLaunch
			return nil
		}
	}
	return fleetPage.startBrowser(m, instance, fleetName)
}

// startBrowser launches (or switches to) the browser for an instance,
// reading the fleet's resolved Fleet-Launch preference. A browser already
// bound to the data dir triggers the switch flow (auto when AutoSwitch is
// on, otherwise a confirm dialog) because Chrome would otherwise forward
// the launch to the existing process and drop our --proxy-server flag.
func (fleetPage *fleetPage) startBrowser(m *model, instance *fleet.Instance, fleetName string) tea.Cmd {
	multiplePerFleet := multipleBrowsersPerFleet(m)
	dataDir := browserDataDir(fleetName, instance.Name, multiplePerFleet)
	instanceBackend := m.instanceBackend(instance)
	instanceKey := fleetName + "/" + instance.Name

	preferFleetLaunch := false
	if f, ok := m.st.Fleets[fleetName]; ok {
		preferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	}

	if _, running := existingBrowserPID(dataDir); running {
		if !multiplePerFleet && m.config != nil && m.config.BrowserSettings.AutoSwitchEnabled() {
			m.message = fmt.Sprintf("Switching browser to %s...", instance.GetDisplayName())
			return switchBrowserCmd(m.portForwards, instanceBackend, instance, instanceKey, dataDir, preferFleetLaunch, "")
		}
		fleetPage.mode = viewConfirmBrowserSwitch
		fleetPage.dialogFleet = fleetName
		fleetPage.dialogInst = instance.Name
		return nil
	}
	m.message = fmt.Sprintf("Starting browser proxy for %s...", instance.GetDisplayName())
	return openBrowserProxyCmd(m.portForwards, instanceBackend, instance, instanceKey, dataDir, preferFleetLaunch, "")
}

// openControlBrowserCmd builds the browser-open command for a control-socket
// "browser.open" request: resolve the instance from instanceKey, then open the
// proxied browser at url.
//
// The subtlety is which instance the existing browser (if any) is proxied to.
// A fleet's instances share one Chrome data dir (unless MultipleBrowsersPerFleet),
// and that one Chrome can only route through a single instance's privoxy proxy
// at a time. There are three cases for the data dir's live browser:
//
//   - none running              → start a fresh proxied browser at url.
//   - running, this instance    → forward url to it as a new tab (Chrome's
//     singleton drops the second --proxy-server flag, but the live process
//     already has the right proxy), which is what a plain openBrowserProxyCmd
//     does.
//   - running, OTHER instance   → switch: kill it and relaunch against THIS
//     instance, then open url. Without this the URL would be forwarded into the
//     other instance's browser, whose proxy can't reach this instance's
//     address — the bug this guards against.
//
// activeBrowser records who the live browser serves (see model). An unknown
// owner (empty) is treated as "different" and switches, which is also correct
// after a host restart: the surviving Chrome's old proxy port is dead, so a
// fresh relaunch is needed regardless.
//
// If the instance can't be found or isn't running, it returns a
// browserProxyMsg carrying the error, which the existing page_fleet.go handler
// surfaces as a status message — no special-casing needed at the call site.
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
	instanceBackend := m.instanceBackend(instance)

	// If a browser is already live for this data dir but proxied to a different
	// instance, switch it over to this one (kill + relaunch) instead of letting
	// Chrome forward the URL into the wrong-proxied process.
	_, running := existingBrowserPID(dataDir)
	if shouldSwitchBrowser(running, m.activeBrowser[dataDir], instanceKey) {
		return switchBrowserCmd(m.portForwards, instanceBackend, instance, instanceKey, dataDir, false, url)
	}
	return openBrowserProxyCmd(m.portForwards, instanceBackend, instance, instanceKey, dataDir, false, url)
}

// shouldSwitchBrowser reports whether a control-socket open for instanceKey must
// switch the live browser (kill + relaunch) rather than reuse it. That is the
// case only when a browser is actually running for the data dir AND it is not
// already proxied to instanceKey — an unknown/empty recorded owner counts as
// "not this instance" so a browser of uncertain origin (e.g. one that survived
// a host restart with a now-dead proxy) is relaunched cleanly rather than
// new-tabbed into.
func shouldSwitchBrowser(running bool, activeInstanceKey, instanceKey string) bool {
	return running && activeInstanceKey != instanceKey
}

// bothBrowserTargets reports whether the workspace's devcontainer.json
// configures both an initialUrl and a Fleet Launch landing page, and
// returns the initialUrl so the chooser dialog can show it. A load error
// is treated as "not both configured".
func bothBrowserTargets(workspaceDir string) (initialURL string, both bool) {
	fc, err := devcontainer.LoadFleetCustomizations(workspaceDir)
	if err != nil {
		return "", false
	}
	initialURL = fc.Browser.InitialURL
	return initialURL, initialURL != "" && fc.FleetLaunch.Configured()
}

// shouldUseLandingPage decides whether the browser should open the Fleet
// Launch landing page rather than browser.initialUrl, given which of the
// two are configured in devcontainer.json and the fleet's PreferFleetLaunch
// setting. The landing page is used when it is configured AND either no
// initialUrl is set or the fleet prefers Fleet Launch. When both are
// configured, preferFleetLaunch is the tie-breaker; when only one is
// configured the setting is irrelevant.
func shouldUseLandingPage(hasURL, hasLanding, preferFleetLaunch bool) bool {
	return hasLanding && (!hasURL || preferFleetLaunch)
}

// switchBrowserCmd kills any browser currently bound to dataDir and then
// runs the normal open-browser-proxy flow against the given instance.
// The two steps are folded into a single tea.Cmd so the UI can keep a
// "Switching..." spinner up for the entire kill+relaunch window and
// tear it down when the resulting browserProxyMsg arrives.
func switchBrowserCmd(
	pf *portforward.Manager,
	instanceBackend backend.Backend,
	instance *fleet.Instance,
	instanceKey string,
	dataDir string,
	preferFleetLaunch bool,
	targetURL string,
) tea.Cmd {
	inner := openBrowserProxyCmd(pf, instanceBackend, instance, instanceKey, dataDir, preferFleetLaunch, targetURL)
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
// Helpers
// ===========================================

// ensureProxyRunning shells into the container and runs the proxy setup
// script. It returns an error if privoxy cannot be installed or started.
func ensureProxyRunning(instanceBackend backend.Backend, workspaceDir string) error {
	cmd := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", proxySetupScript})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("proxy setup: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	result := strings.TrimSpace(string(out))
	// The script echoes a status keyword as its last line of output.
	// Extract it in case earlier lines contain install chatter.
	lines := strings.Split(result, "\n")
	status := lines[len(lines)-1]

	switch {
	case strings.Contains(status, "INSTALL_FAILED"):
		return fmt.Errorf("could not install privoxy (no supported package manager found)")
	case strings.Contains(status, "START_FAILED"):
		return fmt.Errorf("privoxy failed to start — check container logs")
	case strings.Contains(status, "STARTED"):
		// Give the daemon a moment to bind to the port.
		time.Sleep(500 * time.Millisecond)
	}
	// "ALREADY_RUNNING" falls through — nothing extra to do.
	return nil
}

// ensureLandingPageRunning makes sure the in-container fleet-launch
// binary is up to date and the landing-page server it hosts is alive.
// The browser then opens to it through the same privoxy proxy used for
// all container traffic.
//
// Staging the binary is delegated to fleetlaunch.EnsureFresh, which
// handles the version check + copy. The landing-page-specific pieces —
// is the server alive, kill the running server before a refresh so the
// new code actually takes effect, start a fresh server — live here
// because they are scoped to this particular in-container service. The
// fleet-launch binary may host more services later; each will own its
// own pidfile under the /tmp/fleet-launch-* namespace.
func ensureLandingPageRunning(instanceBackend backend.Backend, workspaceDir string) error {
	running, err := landingPageProcessAlive(instanceBackend, workspaceDir)
	if err != nil {
		return err
	}

	// Refresh the staged binary if needed. If the landing-page server is
	// up when we decide to refresh, stop it first — a running process
	// keeps its old executable mmap'd and otherwise wouldn't pick up the
	// new code on the next start.
	refreshed, err := fleetlaunch.EnsureFresh(instanceBackend, workspaceDir, func() error {
		if !running {
			return nil
		}
		if err := stopLandingPage(instanceBackend, workspaceDir); err != nil {
			return err
		}
		running = false
		return nil
	})
	if err != nil {
		return err
	}

	if running && !refreshed {
		return nil
	}

	if err := startLandingPage(instanceBackend, workspaceDir); err != nil {
		return err
	}

	// Give the server a moment to bind before the browser hits it.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// landingPageProcessAlive reports whether the in-container landing-page
// server is still serving — i.e. the pidfile exists and the PID it names
// is alive. A missing pidfile or a stale one (process gone) both read as
// "not running".
func landingPageProcessAlive(instanceBackend backend.Backend, workspaceDir string) (bool, error) {
	check := fmt.Sprintf(`[ -f %s ] && kill -0 "$(cat %s)" 2>/dev/null && echo RUNNING || echo STOPPED`,
		landingPagePidPath, landingPagePidPath)
	out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", check}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check status: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), "RUNNING"), nil
}

// stopLandingPage signals the in-container landing-page server to exit
// and clears its pidfile. It waits briefly for the process to actually
// die so the follow-up start isn't racing the dying server for the port.
// Missing pidfile or already-dead process are both fine — the goal state
// is "no server running", not "we killed something".
func stopLandingPage(instanceBackend backend.Backend, workspaceDir string) error {
	script := fmt.Sprintf(`
pid="$(cat %s 2>/dev/null)"
if [ -n "$pid" ]; then
  kill "$pid" 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
fi
rm -f %s`, landingPagePidPath, landingPagePidPath)
	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput(); err != nil {
		return fmt.Errorf("stop landing page: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startLandingPage launches the in-container fleet binary as the
// landing-page server, detached so it outlives this exec session. The
// workspace flag defaults to "." — the exec runs in the container's
// workspace folder, so the server reads the same devcontainer.json this
// process just resolved.
func startLandingPage(instanceBackend backend.Backend, workspaceDir string) error {
	startScript := fmt.Sprintf(
		`nohup %s landing-page --port %d --workspace . >%s 2>&1 & echo $! > %s`,
		fleetlaunch.RemotePath, landingpage.DefaultPort, landingPageLogPath, landingPagePidPath)
	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", startScript}).CombinedOutput(); err != nil {
		return fmt.Errorf("start server: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// launchBrowser opens a Chromium-based browser with its HTTP/HTTPS/WS/WSS
// traffic routed through the proxy on localPort. The user data directory
// is supplied by the caller so the layout (per-fleet vs per-instance) is
// owned by configuration, not by this function.
//
// initialURL is the page the browser opens to. Callers resolve it from
// the workspace's devcontainer.json fleet customization, falling back to
// defaultBrowserURL when none is configured.
func launchBrowser(localPort int, dataDir, initialURL string) error {
	proxyArg := fmt.Sprintf("--proxy-server=http://localhost:%d", localPort)

	// Chrome won't start if the data dir doesn't exist on first launch
	// (it will, but only sometimes — depends on parent perms). Create
	// it eagerly with the same permissions used elsewhere in fleet-man
	// so subsequent provisioning logic can also write through.
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
				// By default Chrome bypasses the proxy for loopback
				// addresses. The whole point of this feature is to
				// reach services on the container's localhost, so we
				// must disable that bypass.
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

// browserDataDirName is the subdirectory under a fleet (or instance)
// where Chrome's user-data-dir lives.
const browserDataDirName = ".browser"

// browserDataDir returns the host path that Chrome should use as its
// --user-data-dir for an instance. The layout is determined by the
// MultipleBrowsersPerFleet setting:
//
//	false (default): ~/.fleet/workspaces/<fleet>/.browser
//	true:            ~/.fleet/workspaces/<fleet>/<instance>/.browser
//
// Per-fleet keeps bookmarks/passwords shared across instances. Per-instance
// lets two browsers run concurrently for the same fleet.
func browserDataDir(fleetName, instanceName string, multiplePerFleet bool) string {
	if multiplePerFleet {
		return filepath.Join(state.WorkspacesDir(), fleetName, instanceName, browserDataDirName)
	}
	return filepath.Join(state.WorkspacesDir(), fleetName, browserDataDirName)
}

// existingBrowserPID returns the PID of a Chrome process that currently
// owns dataDir, if one is running. It works by reading Chrome's own
// SingletonLock symlink — Chrome creates this on startup with a target
// of "<hostname>-<pid>" and removes it on clean shutdown. A stale lock
// (process gone) returns ok=false so callers don't try to kill nothing.
func existingBrowserPID(dataDir string) (int, bool) {
	target, err := os.Readlink(filepath.Join(dataDir, "SingletonLock"))
	if err != nil {
		return 0, false
	}
	// Format is "<hostname>-<pid>"; split on the last '-' to be safe
	// against hostnames that themselves contain dashes.
	idx := strings.LastIndex(target, "-")
	if idx < 0 || idx == len(target)-1 {
		return 0, false
	}
	pid, err := strconv.Atoi(target[idx+1:])
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Verify the process is actually alive. Signal 0 performs no
	// action but still triggers the kernel's permission/existence
	// checks, so ESRCH means stale.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

// killExistingBrowser terminates the Chrome process that owns dataDir
// and cleans up the singleton metadata files so the next launch is not
// rejected as "another instance is already running". Returns nil if no
// browser was running or after a successful shutdown.
func killExistingBrowser(dataDir string) error {
	pid, ok := existingBrowserPID(dataDir)
	if !ok {
		// Clean up any stale singleton files even when no live PID
		// was found — Chrome occasionally leaves them after a crash
		// and refuses to start until they're gone.
		removeSingletonFiles(dataDir)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find browser process %d: %w", pid, err)
	}
	// SIGTERM lets Chrome shut down cleanly (flushes session state,
	// closes the SQLite DBs). We escalate to SIGKILL only if it
	// doesn't exit within a short window.
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

// removeSingletonFiles clears Chrome's three singleton-tracking symlinks
// from dataDir. Safe to call when they don't exist.
func removeSingletonFiles(dataDir string) {
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}

// multipleBrowsersPerFleet returns the user's preference for whether
// each instance gets its own browser data dir. Falls back to false when
// no config is loaded yet so first-launch behavior matches the documented
// default.
func multipleBrowsersPerFleet(m *model) bool {
	if m.config == nil {
		return false
	}
	return m.config.BrowserSettings.MultipleBrowsersPerFleetEnabled()
}
