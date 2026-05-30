package launchtui

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/appstart"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Update / activation
// ===========================================

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
