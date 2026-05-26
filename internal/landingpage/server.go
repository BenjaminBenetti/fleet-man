// Package landingpage serves fleet-man's built-in browser landing page —
// a directory of links to the dev services running inside an instance.
//
// The page is configured by the customizations.fleet.browser.landingPage
// block in a repo's devcontainer.json. fleet-man injects its own binary
// into the instance and runs it as `fleet landing-page`; that process
// serves this page on a fixed port, and the built-in browser opens to it.
package landingpage

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/gin-gonic/gin"
)

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

	srv := &server{sites: fc.Browser.LandingPage.Sites}
	srv.routes(router, assets)

	addr := ":" + strconv.Itoa(cfg.Port)
	return router.Run(addr)
}

// server holds the request-handling state for the landing page.
type server struct {
	sites []devcontainer.LandingPageSite
}

// routes registers the landing page's HTTP handlers. assets is the
// embedded client-asset filesystem (re-rooted at the static dir), served
// under /static.
func (s *server) routes(r *gin.Engine, assets fs.FS) {
	r.GET("/", s.handleIndex)
	r.StaticFS("/static", http.FS(assets))
}

// handleIndex renders the directory of configured sites.
func (s *server) handleIndex(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", gin.H{
		"Sites": s.sites,
	})
}
