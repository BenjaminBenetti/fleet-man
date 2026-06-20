package tui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"

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

// openExternalURL launches the host OS's default handler for rawURL. WSL is
// special cased (xdg-open there opens nothing useful) to reach the Windows
// browser. The launcher is Run (not Start) so the short-lived process is reaped
// rather than left defunct.
func openExternalURL(rawURL string) error {
	if !isBrowsableURL(rawURL) {
		return fmt.Errorf("refusing to open non-http(s) URL")
	}
	cmd, err := openExternalURLCommand(rawURL)
	if err != nil {
		return err
	}
	return cmd.Run()
}

// openExternalURLCommand picks the per-OS opener. Split out for testability.
// Every opener receives the URL as a separate, non-shell-interpreted argument;
// the WSL PowerShell fallback passes it via the environment rather than
// interpolating it into the -Command string, so a URL containing PowerShell
// metacharacters can't inject code on the host.
func openExternalURLCommand(rawURL string) (*exec.Cmd, error) {
	switch {
	case runtime.GOOS == "darwin":
		return exec.Command("open", rawURL), nil
	case runtime.GOOS == "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL), nil
	case platform.IsWSL():
		// wslu's wslview is the clean path; fall back to the Windows shell's
		// start handler via powershell when it isn't installed.
		if _, err := exec.LookPath("wslview"); err == nil {
			return exec.Command("wslview", rawURL), nil
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Process -FilePath $env:FLEET_OPEN_URL")
		cmd.Env = append(os.Environ(), "FLEET_OPEN_URL="+rawURL)
		return cmd, nil
	case runtime.GOOS == "linux":
		return exec.Command("xdg-open", rawURL), nil
	default:
		return nil, fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
}
