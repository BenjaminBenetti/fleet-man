// Package launchtui is the in-instance Fleet Launch terminal UI run by
// `fleet launch`. It reads the workspace's customizations.fleet.fleetLaunch
// block and presents the configured Links (fleetLaunch.sites) and Apps
// (fleetLaunch.apps) as a flex grid of squares that wraps to the terminal
// width. Arrow keys and hjkl move a cursor; enter or a mouse click activates a
// square.
//
// The browser the TUI drives lives on the host (it is proxied into the
// container by privoxy), so the TUI cannot open it directly. Instead it dials
// the control socket fleet bind-mounts into the instance (internal/control)
// and asks the host fleet TUI to open or navigate the browser:
//
//   - activating a Link sends the link's URL straight to the host;
//   - activating an App first ensures the app's command is running on its port
//     locally (internal/appstart), waits for the port, then sends
//     http://localhost:<port> to the host.
//
// If the control socket cannot be dialled (the host fleet TUI isn't running)
// the program still renders so the user can browse the configured options; it
// shows a persistent status line and refuses to "open" anything until a host
// connection exists.
package launchtui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ===========================================
// Public entry point
// ===========================================

// Config parameterises Run. The zero value is the normal in-instance case:
// auto-detect the workspace's devcontainer.json and dial the standard control
// socket.
type Config struct {
	// ConfigPath is the devcontainer.json to read. Empty means auto-detect by
	// searching the current directory for .devcontainer/devcontainer.json or
	// ./devcontainer.json (LoadFleetCustomizations(".")). A non-empty path is
	// read directly (LoadFleetCustomizationsFromFile) and a missing file is an
	// error.
	ConfigPath string
	// SocketPath overrides the control socket the client dials. Empty means the
	// standard container-side path (control.ContainerSocketPath). Tests set
	// this to a temp socket.
	SocketPath string
}

// Run loads the fleetLaunch configuration, dials the host control socket, and
// runs the grid TUI until the user quits. It returns nil on a clean quit.
//
// Two early exits avoid an empty screen: a configuration that defines neither
// links nor apps prints a friendly note and returns (nothing to show), and a
// failed control dial is NOT fatal — the program runs "degraded" (rendering
// the grid but reporting that opens won't work) so the user can still see what
// is configured.
func Run(cfg Config) error {
	fl, err := loadCustomizations(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if !fl.FleetLaunch.Configured() {
		fmt.Println("Fleet Launch has nothing to show: no links (fleetLaunch.sites) or apps (fleetLaunch.apps) are configured in this devcontainer.json.")
		return nil
	}

	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = control.ContainerSocketPath
	}

	// A dial failure is expected when the host fleet TUI isn't running; keep
	// the client nil and run degraded rather than aborting.
	client, dialErr := control.Dial(socketPath)
	if client != nil {
		defer client.Close()
	}

	m := newModel(buildItems(fl), client, dialErr)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, runErr := prog.Run()
	return runErr
}

// loadCustomizations reads the fleetLaunch configuration from an explicit path
// when one is given, otherwise auto-detects it from the current directory.
func loadCustomizations(configPath string) (devcontainer.FleetCustomizations, error) {
	if configPath != "" {
		return devcontainer.LoadFleetCustomizationsFromFile(configPath)
	}
	return devcontainer.LoadFleetCustomizations(".")
}

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

// Update satisfies tea.Model. It handles the terminal size, keyboard
// navigation/activation, mouse clicks, and the asynchronous result of starting
// an app.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case appOpenedMsg:
		if msg.err != nil {
			m.status = fmt.Sprintf("Could not open %s: %v", msg.title, msg.err)
		} else {
			m.status = fmt.Sprintf("Opening %s…", msg.title)
		}
		return m, nil
	}
	return m, nil
}

// handleKey applies a keypress: quit keys, the four movement directions (arrows
// and hjkl), and enter to activate the cursor.
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	gl := m.grid()
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "left", "h":
		m.cursor = moveLeft(m.cursor, len(m.items))
	case "right", "l":
		m.cursor = moveRight(m.cursor, len(m.items))
	case "up", "k":
		m.cursor = moveUp(gl, m.cursor)
	case "down", "j":
		m.cursor = moveDown(gl, m.cursor)
	case "enter":
		return m.activate(m.cursor)
	}
	return m, nil
}

// handleMouse activates the square under a left-button press. Any other mouse
// activity (movement, other buttons, releases) is ignored. A click both selects
// and activates, matching the "click = open" contract.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	idx := m.grid().hitTest(msg.X, msg.Y)
	if idx < 0 {
		return m, nil
	}
	m.cursor = idx
	return m.activate(idx)
}

// activate opens the item at idx. Both links and apps open via an async
// command so the UI thread never blocks: a link on the socket write to a
// slow/hung host, an app on its command start and the wait for its port to come
// up. When no host connection exists, it sets an error status instead of
// attempting an open.
func (m model) activate(idx int) (tea.Model, tea.Cmd) {
	if idx < 0 || idx >= len(m.items) {
		return m, nil
	}
	it := m.items[idx]

	if m.client == nil {
		m.status = "not connected to host fleet — cannot open (is the host `fleet` TUI running?)"
		return m, nil
	}

	switch it.kind {
	case kindLink:
		m.status = fmt.Sprintf("Opening %s…", it.title)
		return m, openLinkCmd(m.client, it)
	case kindApp:
		m.status = fmt.Sprintf("Starting %s…", it.title)
		return m, openAppCmd(m.client, it)
	}
	return m, nil
}

// openLink asks the host to open a link's URL. It is the shared activation used
// by both the TUI (wrapped in a tea.Cmd) and the headless `fleet launch <name>`
// path.
func openLink(client controlClient, it item) error {
	return client.OpenBrowser(it.url)
}

// openApp starts an app on its port (only if it isn't already answering) and
// then asks the host to open http://localhost:<port>. Shared by the TUI and the
// headless launch path.
func openApp(client controlClient, it item) error {
	if err := appstart.EnsureRunningOnPort(it.command, it.port); err != nil {
		return err
	}
	return client.OpenBrowser(appstart.LocalURL(it.port))
}

// openLinkCmd builds the tea.Cmd that asks the host to open a link's URL. The
// send runs off the UI thread for the same reason openAppCmd does: control.
// Client.Send writes to the unix socket, and a hung or slow host that has
// stopped reading lets the kernel send buffer fill so the write blocks. Doing
// that on the bubbletea Update goroutine would freeze the whole launcher with
// no way to recover; the async command reports the result as an appOpenedMsg
// instead. (Apps already went through openAppCmd; only the link path was
// synchronous.)
func openLinkCmd(client controlClient, it item) tea.Cmd {
	return func() tea.Msg {
		return appOpenedMsg{title: it.title, err: openLink(client, it)}
	}
}

// openAppCmd builds the tea.Cmd that starts an app on its port (only if it
// isn't already answering) and then asks the host to open localhost:<port>.
// All blocking work — the command launch and the wait for the port to come up —
// happens inside the returned function, which Bubble Tea runs on its own
// goroutine, so the UI stays responsive.
func openAppCmd(client controlClient, it item) tea.Cmd {
	return func() tea.Msg {
		return appOpenedMsg{title: it.title, err: openApp(client, it)}
	}
}

// grid resolves the current layout from the latest terminal width and the
// link/app split. It is recomputed on demand (it is cheap) so navigation and
// hit-testing always use geometry that matches the most recent render.
func (m model) grid() gridLayout {
	return layout(m.width, m.items, m.links)
}

// ===========================================
// Rendering
// ===========================================
//
// The stylesheet deliberately mirrors the host TUI's palette (see
// internal/tui/styles.go) so the launcher feels like part of the same app:
// colour 170 (magenta/pink) is the primary accent and selection, 39 (cyan) is
// the section-header/secondary colour, and 241 is the dim help text. Pills are
// drawn as plain coloured text whose colour alternates by global render index
// between pink and cyan so adjacent options are easy to tell apart; only the
// selected pill gets a solid magenta box drawn behind it.

var (
	// headerStyle renders a section header ("Links" / "Apps") in the secondary
	// cyan the host TUI uses for fleet/section headers.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			MarginLeft(horizontalMargin)

	// dividerStyle renders the horizontal rule beneath a section header in the
	// same cyan as the header, separating the label from its pills.
	dividerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")).
			MarginLeft(horizontalMargin)

	// statusStyle renders the bottom status line in the host TUI's dim help
	// colour.
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginLeft(horizontalMargin)

	// degradedStyle renders the status line when no host connection exists, in
	// the host TUI's error red.
	degradedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			MarginLeft(horizontalMargin)

	// pillTextEven / pillTextOdd are the two alternating text colours for
	// unselected pills — the app's two accents, pink and cyan — so neighbouring
	// options are easy to tell apart with no background fill.
	pillTextEven = lipgloss.Color("170") // pink
	pillTextOdd  = lipgloss.Color("39")  // cyan/blue

	// pillSelectedBg / pillSelectedFg style the focused pill as a solid box in
	// the magenta selection accent the host TUI uses, with dark text so it pops
	// against the surrounding plain-text pills.
	pillSelectedBg = lipgloss.Color("170")
	pillSelectedFg = lipgloss.Color("16")
)

// View satisfies tea.Model. It renders the two labelled sections of pills and
// the status/help line. Composition uses lipgloss's ANSI-aware
// JoinHorizontal/JoinVertical rather than a hand-rolled character grid: the
// pills are already styled (their background fills carry escape sequences), and
// only lipgloss's join primitives measure and stack such strings without
// shredding those sequences.
//
// The vertical stack is built to match layout()'s geometry exactly so the
// mouse handler's hit-testing stays in sync: for each section a one-line label
// and a one-line divider (headerRows = 2) followed by its wrapped rows of pills
// (stacked with no gap between rows). Empty sections contribute no pill lines —
// which is also what layout() assumes when it offsets the Apps section by the
// (possibly zero) number of Link rows.
func (m model) View() string {
	if m.width == 0 {
		// No size yet (first frame before WindowSizeMsg); render nothing to
		// avoid a misplaced flash.
		return ""
	}

	gl := m.grid()
	divider := m.divider()

	// "Links" label + divider (headerRows), then its pills.
	parts := []string{
		headerStyle.Render("Links"),
		divider,
	}
	if links := m.renderSection(gl, sectionLink); links != "" {
		parts = append(parts, links)
	}
	// "Apps" label + divider (headerRows), then its pills.
	parts = append(parts, headerStyle.Render("Apps"), divider)
	if apps := m.renderSection(gl, sectionApp); apps != "" {
		parts = append(parts, apps)
	}

	body := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return body + "\n" + m.footer()
}

// renderSection renders one section's pills as a left-indented, wrapping grid:
// each row's pills are joined horizontally (separated by pillGap blank columns)
// and the rows are stacked with no blank line between them (rowStride = 1), so
// the output matches layout()'s geometry exactly. Pills are drawn in flattened
// (global) index order so the alternating fill reads as one continuous pattern
// across both sections. Returns "" when the section has no items, so View can
// omit it and keep the vertical geometry aligned with layout().
func (m model) renderSection(gl gridLayout, sec section) string {
	// Group this section's item indices by their layout row.
	byRow := map[int][]int{}
	maxRow := -1
	for i, p := range gl.placements {
		if p.section != sec {
			continue
		}
		byRow[p.row] = append(byRow[p.row], i)
		if p.row > maxRow {
			maxRow = p.row
		}
	}
	if maxRow < 0 {
		return ""
	}

	gap := strings.Repeat(" ", pillGap)
	indent := strings.Repeat(" ", horizontalMargin)
	var lines []string
	for r := 0; r <= maxRow; r++ {
		var pills []string
		for n, i := range byRow[r] {
			if n > 0 {
				pills = append(pills, gap)
			}
			pills = append(pills, m.renderPill(gl.placements[i], i))
		}
		lines = append(lines, indent+lipgloss.JoinHorizontal(lipgloss.Top, pills...))
	}
	return strings.Join(lines, "\n")
}

// divider renders the horizontal rule shown beneath a section header: a run of
// box-drawing dashes spanning the available content width, in the header's
// cyan, indented to the same left margin as the labels and pills.
func (m model) divider() string {
	w := m.width - 2*horizontalMargin
	if w < 1 {
		w = 1
	}
	return dividerStyle.Render(strings.Repeat("─", w))
}

// renderPill styles the i-th item as a compact single-line pill: just its title,
// no border and no subtitle. Unselected pills are plain coloured text whose
// colour alternates by global index i (pink/cyan) so neighbouring options
// differ; the selected pill instead gets a solid magenta box with dark text.
// Either way the rendered width is the label plus pillPadding on each side
// (padding is whitespace when unselected, the box's interior when selected) —
// exactly the width layout() recorded in the placement's rect, so rendering and
// hit-testing stay in lock-step.
func (m model) renderPill(p itemPlacement, i int) string {
	style := lipgloss.NewStyle().Padding(0, pillPadding)
	switch {
	case i == m.cursor:
		style = style.Background(pillSelectedBg).Foreground(pillSelectedFg).Bold(true)
	case i%2 == 0:
		style = style.Foreground(pillTextEven)
	default:
		style = style.Foreground(pillTextOdd)
	}
	return style.Render(p.label)
}

// footer renders the status line (or the degraded warning) above a one-line key
// hint.
func (m model) footer() string {
	var status string
	switch {
	case m.degrade:
		status = degradedStyle.Render(m.status)
	case m.status != "":
		status = statusStyle.Render(m.status)
	default:
		status = statusStyle.Render(" ")
	}
	help := statusStyle.Render("↑/↓/←/→ or hjkl move · enter/click open · q quit")
	return status + "\n" + help
}

// ===========================================
// Small rendering helpers
// ===========================================

// itemTitle returns the title to show for an item, falling back to the link URL
// (or a generic "App" label) when no explicit title was configured.
func itemTitle(it item) string {
	if it.title != "" {
		return it.title
	}
	if it.kind == kindLink {
		return it.url
	}
	return "App"
}

// localhostHint renders an app's "localhost:<port>" subtitle, or empty when the
// port is unset.
func localhostHint(port int) string {
	if port <= 0 {
		return ""
	}
	return "localhost:" + strconv.Itoa(port)
}

// truncate shortens s to at most max display cells, appending an ellipsis when
// it had to cut. A non-positive max yields the empty string.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
