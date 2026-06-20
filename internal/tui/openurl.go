package tui

import (
	"fmt"
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

// openExternalURLCmd opens url in the system default browser, off the UI thread.
func openExternalURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		return externalURLOpenedMsg{url: url, err: openExternalURL(url)}
	}
}

// openExternalURL launches the host OS's default handler for url. WSL is special
// cased (xdg-open there opens nothing useful) to reach the Windows browser.
func openExternalURL(url string) error {
	cmd, err := openExternalURLCommand(url)
	if err != nil {
		return err
	}
	return cmd.Start()
}

// openExternalURLCommand picks the per-OS opener. Split out for testability.
func openExternalURLCommand(url string) (*exec.Cmd, error) {
	switch {
	case runtime.GOOS == "darwin":
		return exec.Command("open", url), nil
	case runtime.GOOS == "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url), nil
	case platform.IsWSL():
		// wslu's wslview is the clean path; fall back to launching the Windows
		// shell's start handler via powershell when it isn't installed.
		if _, err := exec.LookPath("wslview"); err == nil {
			return exec.Command("wslview", url), nil
		}
		return exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Process", url), nil
	case runtime.GOOS == "linux":
		return exec.Command("xdg-open", url), nil
	default:
		return nil, fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
}
