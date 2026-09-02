package tui

import (
	"fmt"
	"net/url"

	"github.com/BenjaminBenetti/fleet-man/internal/platform"
	tea "github.com/charmbracelet/bubbletea"
)

// openurl.go opens an external URL (e.g. a GitHub PR) in the user's default
// system browser. Unlike browser.go's launchBrowser — which starts a Chromium
// instance proxied INTO a fleet container — this hands a public URL to the host
// OS's URL handler, since the TUI runs on the user's own machine.

// externalURLOpenedMsg reports the result of an openExternalURLCmd.
type externalURLOpenedMsg struct {
	url string
	err error
}

// openExternalURLCmd opens rawURL in the system default browser, off the UI
// thread.
func openExternalURLCmd(rawURL string) tea.Cmd {
	return func() tea.Msg {
		return externalURLOpenedMsg{url: rawURL, err: openExternalURL(rawURL)}
	}
}

// isBrowsableURL reports whether rawURL is an http(s) URL safe to hand to a
// browser opener. PR URLs are always https; rejecting everything else stops a
// hostile in-container gh from getting us to open a file://, javascript:, or
// other surprising scheme on the host.
func isBrowsableURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// openExternalURL launches the host OS's default handler for rawURL — the
// shared platform opener, so a PR link and a `fleet open` file go through the
// same per-OS command. The launcher is Run (not Start) so the short-lived
// process is reaped rather than left defunct.
func openExternalURL(rawURL string) error {
	if !isBrowsableURL(rawURL) {
		return fmt.Errorf("refusing to open non-http(s) URL")
	}
	cmd, err := platform.OpenCommand(rawURL)
	if err != nil {
		return err
	}
	return cmd.Run()
}
