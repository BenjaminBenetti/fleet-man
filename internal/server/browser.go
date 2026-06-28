package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/landingpage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// browser.go owns the CONTAINER-side half of fleet's host-browser feature: the
// privoxy proxy install/start, reading the workspace devcontainer.json
// customization, and staging + running the Fleet Launch landing-page server.
// The HOST-side half (the Chrome process, its data dir, the port forward) stays
// in the thin TUI client (internal/tui/browser.go). This split keeps every
// backend touch server-side (the P5 boundary) while the user's browser still
// launches locally.

// browserProxyPort is the port privoxy listens on inside the container. It is
// hard-coded into privoxyConfig below; the client forwards a local port to it.
const browserProxyPort = 58888

// defaultBrowserURL is the page the browser opens to when the workspace's
// devcontainer.json does not specify an initialUrl.
const defaultBrowserURL = "about:blank"

// landingPagePidPath / landingPageLogPath track the in-container landing-page
// server fleet-launch hosts, namespaced under fleet-launch.
const (
	landingPagePidPath = "/tmp/fleet-launch-landingpage.pid"
	landingPageLogPath = "/tmp/fleet-launch-landingpage.log"
)

// --- Proxy setup script (privoxy) ------------------------------------------
//
// privoxy is used instead of tinyproxy because tinyproxy mishandles responses
// with multiple Set-Cookie headers; privoxy forwards them verbatim.

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
func (s *service) GetBrowserConfig(_ context.Context, req *fleetgrpc.GetBrowserConfigRequest) (*fleetgrpc.GetBrowserConfigReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	reply := &fleetgrpc.GetBrowserConfigReply{}
	if fc, err := devcontainer.LoadFleetCustomizations(inst.WorkspaceDir); err == nil {
		reply.InitialUrl = fc.Browser.InitialURL
		reply.HasLanding = fc.FleetLaunch.Configured()
	}
	return reply, nil
}

// PrepareBrowser ensures the privoxy proxy is running in the container and
// resolves the page the browser should open to. An explicit target_url wins; a
// load error falls back to the default page rather than blocking the launch.
func (s *service) PrepareBrowser(_ context.Context, req *fleetgrpc.PrepareBrowserRequest) (*fleetgrpc.PrepareBrowserReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	b := backendutil.NewForInstance(inst, false)
	wsDir := inst.WorkspaceDir

	if err := ensureProxyRunning(b, wsDir); err != nil {
		flog.Error("browser prepare failed", "fleet", req.GetFleet(), "instance", req.GetInstance(), "err", err)
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	initialURL := defaultBrowserURL
	switch {
	case req.GetTargetUrl() != "":
		initialURL = req.GetTargetUrl()
	default:
		if fc, err := devcontainer.LoadFleetCustomizations(wsDir); err == nil {
			hasURL := fc.Browser.InitialURL != ""
			hasLanding := fc.FleetLaunch.Configured()
			switch {
			case shouldUseLandingPage(hasURL, hasLanding, req.GetPreferFleetLaunch()):
				if err := ensureLandingPageRunning(b, wsDir); err != nil {
					flog.Error("browser prepare failed", "fleet", req.GetFleet(), "instance", req.GetInstance(), "err", err)
					return nil, status.Errorf(codes.Internal, "landing page: %v", err)
				}
				initialURL = appstart.LocalURL(landingpage.DefaultPort)
			case hasURL:
				initialURL = fc.Browser.InitialURL
			}
		}
	}
	flog.Info("browser prepared", "fleet", req.GetFleet(), "instance", req.GetInstance())
	return &fleetgrpc.PrepareBrowserReply{InitialUrl: initialURL}, nil
}

// shouldUseLandingPage decides whether to open the Fleet Launch landing page
// rather than browser.initialUrl, given which of the two are configured and the
// fleet's PreferFleetLaunch setting.
func shouldUseLandingPage(hasURL, hasLanding, preferFleetLaunch bool) bool {
	return hasLanding && (!hasURL || preferFleetLaunch)
}

// ensureProxyRunning shells into the container and runs the proxy setup script.
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
// stale) and makes sure the landing-page server it hosts is alive. A running
// server is stopped before a refresh so the new code takes effect.
func ensureLandingPageRunning(b backend.Backend, workspaceDir string) error {
	running, err := landingPageProcessAlive(b, workspaceDir)
	if err != nil {
		return err
	}
	refreshed, err := fleetlaunch.EnsureFresh(b, workspaceDir, func() error {
		if !running {
			return nil
		}
		if err := stopLandingPage(b, workspaceDir); err != nil {
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
	if err := startLandingPage(b, workspaceDir); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

// landingPageProcessAlive reports whether the in-container landing-page server's
// pidfile names a live process.
func landingPageProcessAlive(b backend.Backend, workspaceDir string) (bool, error) {
	check := fmt.Sprintf(`[ -f %s ] && kill -0 "$(cat %s)" 2>/dev/null && echo RUNNING || echo STOPPED`,
		landingPagePidPath, landingPagePidPath)
	out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", check}).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check status: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), "RUNNING"), nil
}

// stopLandingPage signals the in-container landing-page server to exit and waits
// briefly for it to die so the follow-up start isn't racing it for the port.
func stopLandingPage(b backend.Backend, workspaceDir string) error {
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
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", script}).CombinedOutput(); err != nil {
		return fmt.Errorf("stop landing page: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startLandingPage launches the in-container fleet binary as the landing-page
// server, detached so it outlives this exec session.
func startLandingPage(b backend.Backend, workspaceDir string) error {
	startScript := fmt.Sprintf(
		`nohup %s landing-page --port %d --workspace . >%s 2>&1 & echo $! > %s`,
		fleetlaunch.RemotePath, landingpage.DefaultPort, landingPageLogPath, landingPagePidPath)
	if out, err := b.ExecCommand(workspaceDir, []string{"sh", "-c", startScript}).CombinedOutput(); err != nil {
		return fmt.Errorf("start server: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
