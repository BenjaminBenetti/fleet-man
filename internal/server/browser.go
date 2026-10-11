package server

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/browserproxy"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/landingpage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// browser.go owns the CONTAINER-side half of fleet's host-browser feature: the
// in-instance browser proxy (`fleet browser-proxy`, built into the staged fleet
// binary), reading the workspace devcontainer.json customization, and the
// Fleet Launch landing-page server the same binary hosts. The HOST-side half
// (the Chrome process, its data dir, the port forward) stays in the thin TUI
// client (internal/tui/browser.go). This split keeps every backend touch
// server-side (the P5 boundary) while the user's browser still launches
// locally.
//
// Both in-instance servers are started, pid-tracked and restarted after a
// refresh of the staged binary alike. They listen on every interface where
// the daemon dials the instance's address (Docker), and on 127.0.0.1 where it
// reaches the instance's own loopback (a backend.PortDialer: Kubernetes), so a
// pod never publishes them on its network. The proxy needs no package manager,
// root or egress; a privoxy an older fleet installed and started on the
// proxy's port is stopped first. Only where the instance cannot run the
// daemon's own fleet binary (a macOS daemon's Docker instances, whose staged
// binary is the daemon's darwin build) is privoxy still installed instead.

// browserProxyPort is the port the browser proxy listens on inside the
// container; the client forwards a local port to it.
const browserProxyPort = browserproxy.DefaultPort

// defaultBrowserURL is the page the browser opens to when the workspace's
// devcontainer.json does not specify an initialUrl.
const defaultBrowserURL = "about:blank"

// browserFiles is where the in-instance browser servers keep their pid and log
// files, and the binary that serves them (a var so tests run the scripts
// against a directory of their own).
type browserFiles struct {
	proxyPid, proxyLog     string
	landingPid, landingLog string
	// legacyPid is the pid file of the privoxy older fleets started.
	legacyPid string
	bin       string
}

var browserPaths = browserFiles{
	proxyPid:   "/tmp/fleet-browser-proxy.pid",
	proxyLog:   "/tmp/fleet-browser-proxy.log",
	landingPid: "/tmp/fleet-launch-landingpage.pid",
	landingLog: "/tmp/fleet-launch-landingpage.log",
	legacyPid:  "/tmp/fleet-proxy.pid",
	bin:        fleetlaunch.RemotePath,
}

// browserStartWait bounds a server's start, until its listening line.
const browserStartWait = 10 * time.Second

// builtinBrowserProxy reports whether b's instance runs the built-in proxy:
// it needs the daemon's fleet build in the instance, which a backend that
// stages its own (fleetlaunch.Stager: Kubernetes' pod build) always has, and
// the others get as a copy of the daemon's executable, which runs there only
// when the daemon is Linux. A var for tests.
var builtinBrowserProxy = func(b backend.Backend) bool {
	if _, ok := b.(fleetlaunch.Stager); ok {
		return true
	}
	return runtime.GOOS == "linux"
}

// browserBindAddr is the address the in-instance browser servers listen on:
// 127.0.0.1 where the daemon reaches the instance's own loopback
// (backend.PortDialer), else every interface ("").
func browserBindAddr(b backend.Backend) string {
	if _, ok := b.(backend.PortDialer); ok {
		return "127.0.0.1"
	}
	return ""
}

// --- Legacy proxy setup script (privoxy) -----------------------------------
//
// Only for instances that cannot run the daemon's fleet binary (see
// builtinBrowserProxy). privoxy is used instead of tinyproxy because tinyproxy
// mishandles responses with multiple Set-Cookie headers; privoxy forwards them
// verbatim.

// privoxyEnsureInstalled installs privoxy if not present (sudo fallback across
// package managers).
var privoxyEnsureInstalled = `command -v privoxy >/dev/null 2>&1 || { echo '==> Installing privoxy...'; (apt-get update -qq && apt-get install -y -qq privoxy) 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq privoxy) 2>/dev/null || (apk add privoxy) 2>/dev/null || (sudo apk add privoxy) 2>/dev/null || (dnf install -y privoxy) 2>/dev/null || (sudo dnf install -y privoxy) 2>/dev/null || (yum install -y privoxy) 2>/dev/null || (sudo yum install -y privoxy) 2>/dev/null || { echo 'INSTALL_FAILED'; exit 1; }; }; `

// privoxyAction neutralizes privoxy's privacy/filtering behaviour — fleet only
// wants transparent forwarding to dev services. limit-connect is widened so
// HTTPS CONNECT works for services on any port.
var privoxyAction = `{ -block -filter -handle-as-image -set-image-blocker -hide-from-header -hide-referrer -hide-user-agent -crunch-incoming-cookies -crunch-outgoing-cookies -session-cookies-only -change-x-forwarded-for +limit-connect{1-65535} }\n/\n`

// privoxyConfig is a minimal privoxy configuration listening on browserProxyPort.
var privoxyConfig = `listen-address 0.0.0.0:58888\npermit-access 0.0.0.0/0\ntoggle 0\nenable-remote-toggle 0\nenable-remote-http-toggle 0\nenable-edit-actions 0\nconfdir /etc/privoxy\nactionsfile /tmp/fleet-proxy.action\n`

// proxySetupScript installs privoxy (if missing) and starts it idempotently.
var proxySetupScript = privoxyEnsureInstalled +
	`if [ -f /tmp/fleet-proxy.pid ] && kill -0 $(cat /tmp/fleet-proxy.pid) 2>/dev/null; then echo 'ALREADY_RUNNING'; exit 0; fi; ` +
	`printf '` + privoxyAction + `' > /tmp/fleet-proxy.action; ` +
	`printf '` + privoxyConfig + `' > /tmp/fleet-proxy.conf; ` +
	`privoxy --pidfile /tmp/fleet-proxy.pid /tmp/fleet-proxy.conf && echo 'STARTED' || { echo 'START_FAILED'; exit 1; }`

// GetBrowserConfig reads the workspace's devcontainer.json fleet customization
// so the TUI can decide whether to show the launch chooser. A load error reads
// as "nothing configured".
func (s *service) GetBrowserConfig(ctx context.Context, req *fleetgrpc.GetBrowserConfigRequest) (*fleetgrpc.GetBrowserConfigReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	reply := &fleetgrpc.GetBrowserConfigReply{}
	b := backendutil.NewForInstance(inst, false)
	if fc, err := loadWorkspaceCustomizations(ctx, b, inst.WorkspaceDir); err == nil {
		reply.InitialUrl = fc.Browser.InitialURL
		reply.HasLanding = fc.FleetLaunch.Configured()
	}
	return reply, nil
}

// PrepareBrowser ensures the browser proxy is running in the container and
// resolves the page the browser should open to (starting the landing page
// when that is the page). An explicit target_url wins; a load error falls
// back to the default page rather than blocking the launch.
func (s *service) PrepareBrowser(ctx context.Context, req *fleetgrpc.PrepareBrowserRequest) (*fleetgrpc.PrepareBrowserReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	b := backendutil.NewForInstance(inst, false)
	wsDir := inst.WorkspaceDir
	// The browser's first page load opens a burst of connections through the
	// proxy's forward: a backend whose dials share a connection sets it up
	// meanwhile.
	warmBrowserPorts(b, inst.ContainerID)

	initialURL := defaultBrowserURL
	landing := false
	switch {
	case req.GetTargetUrl() != "":
		initialURL = req.GetTargetUrl()
	default:
		if fc, err := loadWorkspaceCustomizations(ctx, b, wsDir); err == nil {
			hasURL := fc.Browser.InitialURL != ""
			hasLanding := fc.FleetLaunch.Configured()
			switch {
			case shouldUseLandingPage(hasURL, hasLanding, req.GetPreferFleetLaunch()):
				landing = true
				initialURL = appstart.LocalURL(landingpage.DefaultPort)
			case hasURL:
				initialURL = fc.Browser.InitialURL
			}
		}
	}
	if err := ensureBrowserServices(b, wsDir, landing); err != nil {
		flog.Error("browser prepare failed", "fleet", req.GetFleet(), "instance", req.GetInstance(), "err", err)
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	flog.Info("browser prepared", "fleet", req.GetFleet(), "instance", req.GetInstance())
	return &fleetgrpc.PrepareBrowserReply{InitialUrl: initialURL}, nil
}

// browserWarmTimeout bounds warming an instance's ports for a browser open.
const browserWarmTimeout = time.Minute

// warmBrowserPorts sets up b's shared port connection in the background
// (backend.PortWarmer); backends without one have nothing to warm.
func warmBrowserPorts(b backend.Backend, containerID string) {
	w, ok := b.(backend.PortWarmer)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), browserWarmTimeout)
		defer cancel()
		if err := w.WarmPorts(ctx, containerID); err != nil {
			flog.L().Debug("browser: warming the instance's ports", "err", err)
		}
	}()
}

// loadWorkspaceCustomizations reads the workspace devcontainer.json's fleet
// customization through b's WorkspaceReader, so the read reaches the workspace
// wherever the backend keeps it.
func loadWorkspaceCustomizations(ctx context.Context, b backend.Backend, workspaceDir string) (devcontainer.FleetCustomizations, error) {
	reader := backendutil.WorkspaceReaderFor(b)
	return devcontainer.ReadFleetCustomizations(func(relPath string) ([]byte, error) {
		return reader.ReadWorkspaceFile(ctx, workspaceDir, relPath)
	})
}

// shouldUseLandingPage decides whether to open the Fleet Launch landing page
// rather than browser.initialUrl, given which of the two are configured and the
// fleet's PreferFleetLaunch setting.
func shouldUseLandingPage(hasURL, hasLanding, preferFleetLaunch bool) bool {
	return hasLanding && (!hasURL || preferFleetLaunch)
}

// ensureBrowserServices makes sure the in-instance browser proxy, and the
// landing page when wantLanding, run from the current fleet binary: a refresh
// of the staged binary stops both first and restarts them, a server that is
// down is started, and a landing page that was up is kept up. An instance
// that cannot run the daemon's fleet binary gets the privoxy proxy instead.
func ensureBrowserServices(b backend.Backend, workspaceDir string, wantLanding bool) error {
	if !builtinBrowserProxy(b) {
		if err := ensureProxyRunning(b, workspaceDir); err != nil {
			return err
		}
		if wantLanding {
			if err := ensureLandingPageRunning(b, workspaceDir); err != nil {
				return fmt.Errorf("landing page: %w", err)
			}
		}
		return nil
	}
	proxyUp, landingUp, err := browserServicesAlive(b, workspaceDir)
	if err != nil {
		return err
	}
	refreshed, err := fleetlaunch.EnsureFresh(b, workspaceDir, func() error {
		// The new binary must serve: stop what the old one runs.
		if proxyUp {
			if err := stopBrowserServer(b, workspaceDir, browserPaths.proxyPid); err != nil {
				return fmt.Errorf("browser proxy: %w", err)
			}
		}
		if landingUp {
			if err := stopBrowserServer(b, workspaceDir, browserPaths.landingPid); err != nil {
				return fmt.Errorf("landing page: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	addr := browserBindAddr(b)
	if !proxyUp || refreshed {
		if err := startBrowserServer(b, workspaceDir, browserProxyArgs(addr), browserPaths.proxyPid, browserPaths.proxyLog, browserPaths.legacyPid); err != nil {
			return fmt.Errorf("browser proxy: %w", err)
		}
	}
	if (wantLanding && !landingUp) || (landingUp && refreshed) {
		if err := startBrowserServer(b, workspaceDir, landingPageArgs(addr), browserPaths.landingPid, browserPaths.landingLog, ""); err != nil {
			return fmt.Errorf("landing page: %w", err)
		}
	}
	return nil
}

// browserProxyArgs and landingPageArgs are the in-instance fleet commands of
// the two servers, bound to addr ("" for every interface).
func browserProxyArgs(addr string) []string {
	args := []string{"browser-proxy", "--port", strconv.Itoa(browserProxyPort)}
	if addr != "" {
		args = append(args, "--addr", addr)
	}
	return args
}

func landingPageArgs(addr string) []string {
	args := []string{"landing-page", "--port", strconv.Itoa(landingpage.DefaultPort), "--workspace", "."}
	if addr != "" {
		args = append(args, "--addr", addr)
	}
	return args
}

// browserServicesAlive reports whether the proxy's and the landing page's
// pid files name live processes of theirs (a pid a restarted container gave
// to another process does not count), in one exec.
func browserServicesAlive(b backend.Backend, workspaceDir string) (proxy, landing bool, err error) {
	check := fmt.Sprintf(`alive() {
  p="$(cat "$1" 2>/dev/null)"
  if [ -n "$p" ] && kill -0 "$p" 2>/dev/null && { [ ! -r "/proc/$p/cmdline" ] || grep -q -- "$2" "/proc/$p/cmdline" 2>/dev/null; }; then echo RUNNING; else echo STOPPED; fi
}
echo "PROXY $(alive %s browser-proxy) LANDING $(alive %s landing-page)"`,
		backend.ShellQuote(browserPaths.proxyPid), backend.ShellQuote(browserPaths.landingPid))
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", check}).CombinedOutput()
	if err != nil {
		return false, false, fmt.Errorf("check the browser servers: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	text := string(out)
	return strings.Contains(text, "PROXY RUNNING"), strings.Contains(text, "LANDING RUNNING"), nil
}

// startBrowserServer starts the in-instance fleet binary with args, detached
// so it outlives this exec, records its pid and waits for the listening line
// both servers print once they listen. replacePid, when set, is a pid file of
// an earlier server on the same port (an older fleet's privoxy), stopped
// first.
func startBrowserServer(b backend.Backend, workspaceDir string, args []string, pidPath, logPath, replacePid string) error {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, backend.ShellQuote(browserPaths.bin))
	for _, a := range args {
		quoted = append(quoted, backend.ShellQuote(a))
	}
	pid, log := backend.ShellQuote(pidPath), backend.ShellQuote(logPath)
	var script strings.Builder
	if replacePid != "" {
		old := backend.ShellQuote(replacePid)
		fmt.Fprintf(&script, `if [ -f %[1]s ]; then
  old="$(cat %[1]s 2>/dev/null)"
  if [ -n "$old" ]; then
    kill "$old" 2>/dev/null || sudo -n kill "$old" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do kill -0 "$old" 2>/dev/null || break; sleep 0.1; done
  fi
  rm -f %[1]s 2>/dev/null || sudo -n rm -f %[1]s 2>/dev/null || true
fi
`, old)
	}
	// The log is emptied first: the background job's own redirect lands
	// after this script reads it, and an earlier run's listening line must
	// not count.
	fmt.Fprintf(&script, `: > %[2]s
nohup %[1]s >>%[2]s 2>&1 </dev/null &
echo $! > %[3]s
pid=$!
i=0
while [ "$i" -lt %[4]d ]; do
  if grep -q 'listening on' %[2]s 2>/dev/null; then echo STARTED; exit 0; fi
  if ! kill -0 "$pid" 2>/dev/null; then echo START_FAILED; cat %[2]s; exit 1; fi
  sleep 0.1
  i=$((i+1))
done
echo START_FAILED; cat %[2]s; exit 1
`, strings.Join(quoted, " "), log, pid, int(browserStartWait/(100*time.Millisecond)))
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", script.String()}).CombinedOutputWithTimeout(browserStartWait + 30*time.Second)
	text := strings.TrimSpace(string(out))
	if err != nil || !strings.Contains(text, "STARTED") {
		if detail := strings.TrimSpace(strings.TrimPrefix(text, "START_FAILED")); detail != "" {
			return fmt.Errorf("did not start: %s", detail)
		}
		return fmt.Errorf("did not start: %v", err)
	}
	return nil
}

// stopBrowserServer signals the server pidPath names to exit and waits
// briefly for it to die, so a following start is not racing it for the port.
func stopBrowserServer(b backend.Backend, workspaceDir, pidPath string) error {
	script := fmt.Sprintf(`
pid="$(cat %[1]s 2>/dev/null)"
if [ -n "$pid" ]; then
  kill "$pid" 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
fi
rm -f %[1]s`, backend.ShellQuote(pidPath))
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput(); err != nil {
		return fmt.Errorf("stop: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureProxyRunning shells into the container and runs the privoxy setup
// script (an instance that cannot run the built-in proxy).
func ensureProxyRunning(b backend.Backend, workspaceDir string) error {
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", proxySetupScript}).CombinedOutput()
	if err != nil {
		return fmt.Errorf("proxy setup: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
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
	// "ALREADY_RUNNING" falls through.
	return nil
}

// ensureLandingPageRunning stages the in-container fleet-launch binary (when
// stale) and makes sure the landing-page server it hosts is alive: the
// landing page of an instance that runs the privoxy proxy. A running server
// is stopped before a refresh so the new code takes effect.
func ensureLandingPageRunning(b backend.Backend, workspaceDir string) error {
	_, running, err := browserServicesAlive(b, workspaceDir)
	if err != nil {
		return err
	}
	refreshed, err := fleetlaunch.EnsureFresh(b, workspaceDir, func() error {
		if !running {
			return nil
		}
		if err := stopBrowserServer(b, workspaceDir, browserPaths.landingPid); err != nil {
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
	return startBrowserServer(b, workspaceDir, landingPageArgs(""), browserPaths.landingPid, browserPaths.landingLog, "")
}
