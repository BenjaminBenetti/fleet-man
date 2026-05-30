package launchtui

import (
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Messages
// ===========================================

// appOpenedMsg reports the outcome of activating an App square: the result of
// starting it on its port and asking the host to open localhost:<port>. title
// is the app's label for the status line; err is non-nil when the app never
// came up or the host refused the open.
type appOpenedMsg struct {
	title string
	err   error
}

// ===========================================
// Model
// ===========================================

// model is the Bubble Tea model for the launcher grid. It owns the flattened
// item list, the cursor, the most recent terminal size (for layout), the
// control client (nil when running degraded), and a single status line.
type model struct {
	items   []item
	links   int
	cursor  int
	width   int
	height  int
	client  controlClient
	degrade bool
	status  string
}

// controlClient is the slice of *control.Client the model uses. Defining it as
// an interface keeps the model testable and documents the exact surface the
// TUI depends on, but in production it is always a *control.Client.
type controlClient interface {
	OpenBrowser(url string) error
}

// newModel builds the initial model from the flattened items and a (possibly
// nil) control client. dialErr, when non-nil, puts the model into degraded
// mode with an explanatory status line so the user understands why opening is
// disabled.
func newModel(items []item, client *control.Client, dialErr error) model {
	m := model{
		items: items,
		links: linkCount(items),
	}
	// A typed-nil *control.Client stored in an interface is NOT == nil, so only
	// assign the interface when the concrete client is real; otherwise leave
	// m.client nil and run degraded.
	if client != nil {
		m.client = client
	}
	if dialErr != nil || client == nil {
		m.degrade = true
		m.status = "not connected to host fleet — open will not work (is the host `fleet` TUI running?)"
	}
	return m
}

// Init satisfies tea.Model. The launcher has no startup command; layout and
// rendering wait for the first WindowSizeMsg.
func (m model) Init() tea.Cmd {
	return nil
}

// grid resolves the current layout from the latest terminal width and the
// link/app split. It is recomputed on demand (it is cheap) so navigation and
// hit-testing always use geometry that matches the most recent render.
func (m model) grid() gridLayout {
	return layout(m.width, m.items, m.links)
}
