package launchtui

import "github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"

// ===========================================
// Selectable items
// ===========================================
//
// The two configured lists — fleetLaunch.sites (links) and fleetLaunch.apps —
// are flattened into a single ordered slice of items: every link first, then
// every app. That flat order is the cursor's index space and the render order,
// so a single integer addresses both "which square is highlighted" and "which
// square am I drawing". Each item keeps just enough of its source config to
// render a square and to activate it (open a URL, or start a command then open
// localhost:port).

// itemKind distinguishes the two kinds of selectable squares so activation can
// branch: a link opens a URL directly, an app must be started locally first.
type itemKind int

const (
	// kindLink is a fleetLaunch.sites entry: activating it opens site.URL.
	kindLink itemKind = iota
	// kindApp is a fleetLaunch.apps entry: activating it ensures the command is
	// running on its port, then opens http://localhost:<port>.
	kindApp
)

// item is one selectable square in the grid. title/subtitle are what the square
// renders; the activation fields carry whichever target the kind needs (url for
// a link, command+port for an app). Fields not relevant to the kind are zero.
type item struct {
	// kind selects link vs app behaviour on activation.
	kind itemKind
	// title is the square's primary label.
	title string
	// subtitle is the square's secondary line (a link's subtitle, or an app's
	// localhost:port hint). May be empty.
	subtitle string
	// url is the link target. Set only for kindLink.
	url string
	// command is the app's start command. Set only for kindApp; may be empty
	// when the app is expected to already be running.
	command string
	// port is the app's localhost port. Set only for kindApp.
	port int
}

// buildItems flattens a FleetCustomizations into the ordered item slice: all
// link entries (in config order) followed by all app entries (in config
// order). This ordering is load-bearing — layout() is told the link count and
// app count and assumes exactly this links-then-apps arrangement, and the
// cursor indexes into the same slice.
func buildItems(fl devcontainer.FleetCustomizations) []item {
	items := make([]item, 0, len(fl.FleetLaunch.Sites)+len(fl.FleetLaunch.Apps))
	for _, site := range fl.FleetLaunch.Sites {
		items = append(items, item{
			kind:     kindLink,
			title:    site.Title,
			subtitle: site.SubTitle,
			url:      site.URL,
		})
	}
	for _, app := range fl.FleetLaunch.Apps {
		items = append(items, item{
			kind:     kindApp,
			title:    app.Title,
			subtitle: localhostHint(app.Port),
			command:  app.Command,
			port:     app.Port,
		})
	}
	return items
}

// linkCount returns how many leading items are links, which is the boundary
// layout() needs between its two sections. It counts the contiguous run of
// kindLink items at the front (buildItems guarantees links precede apps).
func linkCount(items []item) int {
	n := 0
	for _, it := range items {
		if it.kind != kindLink {
			break
		}
		n++
	}
	return n
}
