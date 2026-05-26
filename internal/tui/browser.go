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

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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

// landingPageRemotePath is where fleet-man's own binary is copied inside
// the container to serve the browser landing page, and landingPagePidPath
// tracks the running server's PID for idempotent (re)starts.
const (
	landingPageRemotePath = "/tmp/fleet-landing-page"
	landingPagePidPath    = "/tmp/fleet-landing-page.pid"
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
func openBrowserProxyCmd(
	pf *portforward.Manager,
	instanceBackend backend.Backend,
	instance *fleet.Instance,
	instanceKey string,
	dataDir string,
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
		//    devcontainer.json fleet customization. Precedence:
		//      a) browser.initialUrl — if set, it always wins and the
		//         landing page does not activate.
		//      b) otherwise, if a landing page is configured, inject and
		//         start fleet-man's own binary as the landing-page server
		//         in the container and open to it.
		//      c) otherwise, the default page.
		//    A missing or malformed config (load error) falls back to the
		//    default rather than blocking the launch.
		initialURL := defaultBrowserURL
		if fc, err := devcontainer.LoadFleetCustomizations(workspaceDir); err == nil {
			switch {
			case fc.Browser.InitialURL != "":
				initialURL = fc.Browser.InitialURL
			case len(fc.Browser.LandingPage.Sites) > 0:
				if err := ensureLandingPageRunning(instanceBackend, workspaceDir); err != nil {
					return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("landing page: %w", err)}
				}
				initialURL = fmt.Sprintf("http://localhost:%d", landingpage.DefaultPort)
			}
		}
		if err := launchBrowser(localPort, dataDir, initialURL); err != nil {
			return browserProxyMsg{instanceKey: instanceKey, err: fmt.Errorf("launch browser: %w", err)}
		}

		return browserProxyMsg{instanceKey: instanceKey, localPort: localPort}
	}
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
) tea.Cmd {
	inner := openBrowserProxyCmd(pf, instanceBackend, instance, instanceKey, dataDir)
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

// ensureLandingPageRunning injects fleet-man's own binary into the
// container and starts it as `fleet landing-page` on the landing page
// port. The browser then opens to it through the same privoxy proxy used
// for all container traffic.
//
// It is idempotent: when a landing page process is already running
// (tracked via pidfile) it returns without re-copying the binary, which
// also avoids the ETXTBSY that would result from overwriting the running
// executable. fleet-man only ever serves one landing page per container,
// so the fixed /tmp paths are safe.
func ensureLandingPageRunning(instanceBackend backend.Backend, workspaceDir string) error {
	// Already running? Leave it be.
	check := fmt.Sprintf(`[ -f %s ] && kill -0 "$(cat %s)" 2>/dev/null && echo RUNNING || echo STOPPED`,
		landingPagePidPath, landingPagePidPath)
	out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", check}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("check status: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if strings.Contains(string(out), "RUNNING") {
		return nil
	}

	// Stream this fleet binary into the container over stdin. devcontainer
	// exec allocates no TTY, so the bytes pass through unmangled.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate fleet binary: %w", err)
	}
	bin, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open fleet binary: %w", err)
	}
	defer bin.Close()

	copyScript := fmt.Sprintf(`cat > %s && chmod +x %s`, landingPageRemotePath, landingPageRemotePath)
	copyCmd := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", copyScript})
	copyCmd.Stdin = bin
	if out, err := copyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy fleet binary: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Start it detached on the landing page port. nohup + redirected stdio
	// lets the server outlive this exec session; the workspace defaults to
	// the exec's working dir (the container's workspace folder), so the
	// server reads the same devcontainer.json fleet just loaded.
	startScript := fmt.Sprintf(
		`nohup %s landing-page --port %d --workspace . >/tmp/fleet-landing-page.log 2>&1 & echo $! > %s`,
		landingPageRemotePath, landingpage.DefaultPort, landingPagePidPath)
	if out, err := instanceBackend.ExecCommand(workspaceDir, []string{"sh", "-c", startScript}).CombinedOutput(); err != nil {
		return fmt.Errorf("start server: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Give the server a moment to bind before the browser hits it.
	time.Sleep(500 * time.Millisecond)
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
