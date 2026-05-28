// Package landingpage serves fleet-man's built-in Fleet Launch page —
// a directory of links to the dev services running inside an instance.
//
// The page is configured by the customizations.fleet.fleetLaunch block
// in a repo's devcontainer.json. fleet-man injects its own binary into
// the instance and runs it as `fleet landing-page`; that process serves
// this page on a fixed port, and the built-in browser opens to it.
package landingpage

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/gin-gonic/gin"
)

// healthTimeout caps each upstream health probe so a slow or hung service
// can't tie up the poll request.
const healthTimeout = 5 * time.Second

// DefaultPort is the port the landing page listens on inside the
// instance. fleet-man's browser opens to http://localhost:<DefaultPort>,
// routed through the privoxy proxy into the container's localhost.
const DefaultPort = 16767

//go:embed templates/*.html
var templatesFS embed.FS

// staticFS holds client-side assets (htmx, etc.) served under /static.
// They are vendored and embedded rather than loaded from a CDN: the page
// is served inside the instance and reached through the privoxy proxy, so
// it cannot assume public internet access — and embedding keeps the whole
// feature inside the single fleet binary.
//
//go:embed static/*
var staticFS embed.FS

// Config holds the resolved inputs the server needs to run.
type Config struct {
	// Port is the TCP port to listen on.
	Port int
	// WorkspaceDir is the directory whose devcontainer.json supplies the
	// landing-page site list. The injected process runs with its working
	// directory at the instance's workspace folder, so this is "." in the
	// normal case.
	WorkspaceDir string
}

// Run loads the landing-page configuration from the workspace's
// devcontainer.json and serves the page until the process is killed.
// It blocks; callers (the `fleet landing-page` subcommand) run it as the
// foreground of the injected process.
func Run(cfg Config) error {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = "."
	}

	// The site list is read once at startup. fleet-man re-injects and
	// restarts the process when it relaunches the browser, so a config
	// change is picked up on the next browser open rather than live.
	fc, err := devcontainer.LoadFleetCustomizations(cfg.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("load landing-page customizations: %w", err)
	}

	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}

	// Re-root the embedded assets at the "static" dir so a request for
	// /static/htmx.min.js maps to static/htmx.min.js rather than the
	// doubled-up static/static/... the raw embed.FS would expect.
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("sub static fs: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.SetHTMLTemplate(tmpl)

	srv := &server{
		sites:  fc.FleetLaunch.Sites,
		apps:   fc.FleetLaunch.Apps,
		client: &http.Client{Timeout: healthTimeout},
	}
	srv.routes(router, assets)

	addr := ":" + strconv.Itoa(cfg.Port)
	return router.Run(addr)
}

// server holds the request-handling state for the landing page.
type server struct {
	sites  []devcontainer.FleetLaunchSite
	apps   []devcontainer.FleetLaunchApp
	client *http.Client
}

// routes registers the landing page's HTTP handlers. assets is the
// embedded client-asset filesystem (re-rooted at the static dir), served
// under /static.
func (s *server) routes(r *gin.Engine, assets fs.FS) {
	r.GET("/", s.handleIndex)
	r.GET("/health/:i", s.handleHealth)
	r.GET("/app/:i", s.handleApp)
	r.StaticFS("/static", http.FS(assets))
}

// handleIndex renders the directory of configured sites and app tabs.
func (s *server) handleIndex(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", gin.H{
		"Sites": s.sites,
		"Apps":  s.apps,
	})
}

// healthResult is the resolved state of one site's health probe, rendered
// into the health.html fragment that htmx polls for.
type healthResult struct {
	// Healthy is true when the probe got an HTTP response with a status
	// below 400.
	Healthy bool
	// Label is the hover text: the HTTP status (e.g. "200 OK") or a short
	// failure reason when no response came back.
	Label string
}

// handleHealth probes a single site's healthCheck URL and returns the
// health.html fragment (heart/skull + status label) for htmx to swap in.
//
// The site is addressed by its index in the configured list rather than by
// a URL in the request: the server only ever probes URLs it loaded from the
// devcontainer.json, so a client can't coerce it into fetching an arbitrary
// address (no SSRF surface).
func (s *server) handleHealth(c *gin.Context) {
	i, err := strconv.Atoi(c.Param("i"))
	if err != nil || i < 0 || i >= len(s.sites) {
		c.Status(http.StatusNotFound)
		return
	}
	site := s.sites[i]
	if site.HealthCheck == "" {
		c.Status(http.StatusNoContent)
		return
	}

	c.HTML(http.StatusOK, "health.html", s.probeHealth(site.HealthCheck))
}

// probeHealth issues a GET to url and classifies the outcome. Any response
// with a status below 400 is healthy; a transport error (DNS, refused,
// timeout) or a 4xx/5xx is unhealthy.
func (s *server) probeHealth(url string) healthResult {
	resp, err := s.client.Get(url)
	if err != nil {
		return healthResult{Healthy: false, Label: "unreachable"}
	}
	defer resp.Body.Close()

	label := strconv.Itoa(resp.StatusCode)
	if text := http.StatusText(resp.StatusCode); text != "" {
		label += " " + text
	}
	return healthResult{Healthy: resp.StatusCode < 400, Label: label}
}

// handleApp starts a configured app (if it isn't already up) and returns
// the app.html fragment — an iframe pointing at the app, or an error.
// htmx loads this into the app's tab the first time the tab is clicked.
//
// The app is addressed by its index in the configured list rather than by
// a command or port in the request: the server only ever runs commands and
// iframes ports it loaded from the devcontainer.json, so a client can't
// coerce it into executing arbitrary commands.
func (s *server) handleApp(c *gin.Context) {
	i, err := strconv.Atoi(c.Param("i"))
	if err != nil || i < 0 || i >= len(s.apps) {
		c.Status(http.StatusNotFound)
		return
	}
	app := s.apps[i]
	url := fmt.Sprintf("http://localhost:%d", app.Port)

	if err := ensureAppRunning(app, url); err != nil {
		c.HTML(http.StatusOK, "app.html", gin.H{"Title": app.Title, "Err": err.Error()})
		return
	}
	// Hold the request open until the app is actually answering, so the
	// browser's first paint isn't a connection-refused page.
	if !waitForReachable(url) {
		c.HTML(http.StatusOK, "app.html", gin.H{
			"Title": app.Title,
			"Err":   fmt.Sprintf("did not become reachable on port %d", app.Port),
		})
		return
	}
	c.HTML(http.StatusOK, "app.html", gin.H{"Title": app.Title, "URL": url})
}

// ensureAppRunning starts the app's command unless its port is already
// answering. It is idempotent: an app already serving on its port is left
// alone (so a second tab open or a browser relaunch doesn't double-start
// it), and an app with no command relies entirely on the port poll.
//
// The command is started in the background and not waited on — it is
// expected to be a long-running server (or to detach itself, e.g.
// `docker run -d`). Readiness is determined by polling the port, not by
// the command's exit, so a server that never binds the port surfaces as a
// "did not become reachable" error from handleApp rather than a hang.
func ensureAppRunning(app devcontainer.FleetLaunchApp, url string) error {
	if reachable(url) {
		return nil // already running
	}
	if strings.TrimSpace(app.Command) == "" {
		return nil // nothing to start; rely on the port poll
	}
	cmd := exec.Command("bash", "-c", app.Command)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}
	return nil
}

// reachable reports whether url answers an HTTP request within a short
// timeout. Any response (even an error status) counts as reachable — the
// app's own server is up.
func reachable(url string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// waitForReachable polls url until it answers or a deadline passes,
// reporting whether it came up. Used to hold an app-tab request open until
// the app's HTTP server is listening (it can take a moment to start after
// its command is launched).
func waitForReachable(url string) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if reachable(url) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
