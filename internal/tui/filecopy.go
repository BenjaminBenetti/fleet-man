package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// copyInstanceFileCmd pulls fleet/instance:path to this machine off the UI
// thread — to the requested destination when the in-instance sender named one,
// the local downloads folder otherwise.
func copyInstanceFileCmd(fleetName, instanceName, path, requestedDest string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fileCopyTimeout)
		defer cancel()
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			return fileCopyDoneMsg{path: path, err: err}
		}
		defer conn.Close()
		dest, _, err := fleetclient.CopyFileTo(ctx, conn.Service(), fleetName, instanceName, path, resolveRequestedDest(requestedDest))
		if err != nil {
			return fileCopyDoneMsg{path: path, err: err}
		}
		return fileCopyDoneMsg{path: path, dest: dest}
	}
}

// resolveRequestedDest maps the dest string typed inside the instance onto
// this machine's filesystem, scp-style: empty means the downloads folder, an
// absolute path is used as-is, and `~/` or a relative path resolve against
// this user's home — the instance's cwd means nothing here, so home is the
// only sensible anchor (it is also what scp does for relative remote paths).
func resolveRequestedDest(dest string) string {
	if dest == "" {
		return downloadsDir()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dest
	}
	switch {
	case dest == "~":
		return home
	case strings.HasPrefix(dest, "~/"):
		return filepath.Join(home, dest[2:])
	case !filepath.IsAbs(dest):
		return filepath.Join(home, dest)
	}
	return dest
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
