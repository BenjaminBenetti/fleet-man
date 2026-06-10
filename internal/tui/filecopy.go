package tui

import (
	"context"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// filecopy.go is the client half of the in-instance `fleet copy` (fc) feature:
// a file.copy control envelope arrives as a Watch FileCopy event
// (watchFileCopyMsg), and THIS client pulls the bytes over the CopyFile RPC and
// writes them to its local downloads folder. The split mirrors BrowserOpen —
// the server only names the file; the machine the user is sitting at fetches
// it, which is what makes fc deliver to the local machine even when the fleet
// server (and the instance) are fully remote.

// fileCopyTimeout bounds one pull. Generous — a copied artifact can be a large
// build output crossing a WAN tunnel — while still unsticking a wedged stream.
const fileCopyTimeout = 15 * time.Minute

// fileCopyDoneMsg reports a finished (or failed) background file pull.
type fileCopyDoneMsg struct {
	path string // in-instance source path (for the failure message)
	dest string // local path written (empty on failure)
	err  error
}

// copyInstanceFileCmd pulls fleet/instance:path to the local downloads folder
// off the UI thread.
func copyInstanceFileCmd(fleetName, instanceName, path string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fileCopyTimeout)
		defer cancel()
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			return fileCopyDoneMsg{path: path, err: err}
		}
		defer conn.Close()
		dest, _, err := fleetclient.CopyFileTo(ctx, conn.Service(), fleetName, instanceName, path, downloadsDir())
		if err != nil {
			return fileCopyDoneMsg{path: path, err: err}
		}
		return fileCopyDoneMsg{path: path, dest: dest}
	}
}

// downloadsDir picks where fc-copied files land: ~/Downloads when it exists
// (the conventional per-user download folder), otherwise the home directory,
// falling back to the current directory when even home is unknown.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	downloads := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(downloads); err == nil && fi.IsDir() {
		return downloads
	}
	return home
}
