package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/platform"
)

// filecopy.go is the client half of the in-instance `fleet copy` (fc) feature:
// a file.copy control envelope arrives as a Watch FileCopy event
// (watchFileCopyMsg) naming two scp endpoints, and THIS client runs the generic
// copy engine against the host fleet with its own disk as the "local" side. The
// split mirrors BrowserOpen — the in-container caller can't reach the host fleetd
// or the user's disk, so the machine the user is sitting at performs the copy,
// which is what makes fc work (in either direction) even when the fleet server
// and the instance are fully remote.

// fileCopyTimeout bounds one copy. Generous — a copied artifact can be a large
// build output crossing a WAN tunnel — while still unsticking a wedged stream.
const fileCopyTimeout = 15 * time.Minute

// fileCopyDoneMsg reports a finished (or failed) background copy — and, for a
// fleet open, whether the delivered file was opened.
type fileCopyDoneMsg struct {
	src     string // source endpoint as typed inside the instance (for messages)
	dst     string // destination endpoint as typed inside the instance
	dest    string // final path written (empty on failure)
	err     error  // the copy itself failed
	opened  bool   // the file was handed to the default application
	openErr error  // the copy landed but opening it failed
}

// statusLine renders the outcome for the status bar.
func (msg fileCopyDoneMsg) statusLine() string {
	switch {
	case msg.err != nil:
		return fmt.Sprintf("Copy of %s failed: %v", msg.src, msg.err)
	case errors.Is(msg.openErr, platform.ErrLauncher):
		// The error names the path too; the line already says where it landed.
		return fmt.Sprintf("Copied %s -> %s, but not opened: %v", msg.src, msg.dest, platform.ErrLauncher)
	case msg.openErr != nil:
		return fmt.Sprintf("Copied %s -> %s, but could not open it: %v", msg.src, msg.dest, msg.openErr)
	case msg.opened:
		return fmt.Sprintf("Opened %s (%s)", msg.src, msg.dest)
	default:
		return fmt.Sprintf("Copied %s -> %s", msg.src, msg.dest)
	}
}

// copyForInstanceCmd runs the copy an in-instance `fc` (or `fo`) delegated, off
// the UI thread. req.src/dst are the endpoints as typed inside the instance;
// req.fleet/instance identify the sender, used to resolve a `:path` (self)
// endpoint and to default the fleet of a bare instance reference. For a fleet
// open the delivered file is then handed to this machine's default application.
func copyForInstanceCmd(req copyRequest) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fileCopyTimeout)
		defer cancel()
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			return fileCopyDoneMsg{src: req.src, dst: req.dst, err: err}
		}
		defer conn.Close()
		dstEnd := resolveTUIEndpoint(req.dst, req.fleet, req.instance)
		res, err := fleetclient.Copy(ctx, conn.Service(),
			resolveTUIEndpoint(req.src, req.fleet, req.instance),
			dstEnd,
			tuiLocalPolicy{})
		if err != nil {
			return fileCopyDoneMsg{src: req.src, dst: req.dst, err: err}
		}
		done := fileCopyDoneMsg{src: req.src, dst: req.dst, dest: res.DestPath}
		if req.open {
			done.dest, done.openErr = openDelivered(dstEnd, res.DestPath)
			done.opened = done.openErr == nil
		}
		return done
	}
}

// openDelivered hands a just-copied file to this machine's default application
// and returns the (absolute) path it reported. It refuses when the destination
// was not this machine: the path would then be an in-instance one, and opening
// a same-named local file would be both wrong and a way for a crafted envelope
// to open arbitrary host paths. platform.OpenFile additionally refuses anything
// the opener would launch rather than view (executables, .desktop, .app, ...).
func openDelivered(dst fleetclient.ResolvedEndpoint, dest string) (string, error) {
	if !dst.Local {
		return dest, errors.New("destination is not on this machine")
	}
	return platform.OpenFile(dest)
}

// copyDstLabel renders a destination for the status line — the empty 1-arg
// download shorthand reads as the downloads folder.
func copyDstLabel(dst string) string {
	if dst == "" {
		return "your downloads folder"
	}
	return dst
}

// resolveTUIEndpoint turns a typed endpoint into a ResolvedEndpoint for a copy
// the TUI runs on a sender's behalf: a plain path is local (this machine), `:path`
// is the sender's own instance, and a bare instance reference defaults to the
// sender's fleet (a sibling in the user's fleet).
func resolveTUIEndpoint(arg, origFleet, origInstance string) fleetclient.ResolvedEndpoint {
	ep := fleetclient.ParseCopyEndpoint(arg)
	switch ep.Kind {
	case fleetclient.CopySelf:
		return fleetclient.ResolvedEndpoint{Fleet: origFleet, Instance: origInstance, Path: ep.Path}
	case fleetclient.CopyInstance:
		fleetName := ep.Fleet
		if fleetName == "" {
			fleetName = origFleet
		}
		return fleetclient.ResolvedEndpoint{Fleet: fleetName, Instance: ep.Instance, Path: ep.Path}
	default:
		// CopyHost is this (the host TUI's) machine; CopyLocal shouldn't reach here
		// (the in-instance `fc` rewrites its plain paths to `:` self endpoints), but
		// fall back to the host's disk for it too — the empty 1-arg dst lands here.
		return fleetclient.ResolvedEndpoint{Local: true, Path: ep.Path}
	}
}

// tuiLocalPolicy resolves typed local paths on the human's machine for a
// delegated in-instance copy. The instance's cwd means nothing here, so a
// relative or `~/` path resolves against this user's home (what scp does for
// relative remote paths); an empty destination lands in the downloads folder; a
// directory destination keeps the source's name.
type tuiLocalPolicy struct{}

func (tuiLocalPolicy) ResolveSrc(path string) string { return resolveLocalPath(path) }

func (tuiLocalPolicy) ResolveDest(dest, name string) (string, error) {
	if dest == "" {
		return filepath.Join(downloadsDir(), name), nil
	}
	resolved := resolveLocalPath(dest)
	if strings.HasSuffix(dest, "/") {
		return filepath.Join(resolved, name), nil
	}
	if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
		return filepath.Join(resolved, name), nil
	}
	return resolved, nil
}

// resolveLocalPath maps a typed local path onto this machine's filesystem,
// scp-style: an absolute path is used as-is, while `~/` and relative paths
// resolve against this user's home — the only sensible anchor, since the
// instance's cwd is meaningless on this machine.
func resolveLocalPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	case !filepath.IsAbs(path):
		return filepath.Join(home, path)
	}
	return path
}

// downloadsDir picks where a 1-arg `fc` download lands: ~/Downloads when it
// exists (the conventional per-user download folder), otherwise the home
// directory, falling back to the current directory when even home is unknown.
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
