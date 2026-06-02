package tui

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/version"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// releaseNotesMsg carries the GitHub release notes for the currently
// running version, fetched on startup when the user has upgraded. An
// empty version means "nothing to show" (dev build, already seen,
// network error, or no release body) and is silently ignored.
type releaseNotesMsg struct {
	version string // tag of the running version, e.g. "v1.2.3"
	body    string // release description (markdown) from GitHub
}

// checkReleaseNotesCmd fetches the GitHub release notes for the compiled-in
// version when it differs from the last version the user has seen. It is a
// no-op for dev builds (no version) and when the user has already seen the
// current version's notes — in both cases it returns an empty message.
func checkReleaseNotesCmd(lastSeen string) tea.Cmd {
	return func() tea.Msg {
		current := version.Version
		if current == "" {
			// Dev build — no release to describe.
			return releaseNotesMsg{}
		}
		if current == lastSeen {
			// User has already seen this version's notes.
			return releaseNotesMsg{}
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("https://api.github.com/repos/BenjaminBenetti/fleet-man/releases/tags/" + current)
		if err != nil {
			return releaseNotesMsg{}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return releaseNotesMsg{}
		}

		var release struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			return releaseNotesMsg{}
		}

		return releaseNotesMsg{version: current, body: release.Body}
	}
}

// releaseNotesShowing reports whether the release notes overlay is active.
func (m *model) releaseNotesShowing() bool {
	return m.releaseNotesVersion != ""
}

// lastSeenVersion returns the version whose release notes the user has
// already seen, or "" if state is unavailable.
func (m *model) lastSeenVersion() string {
	if m.st == nil {
		return ""
	}
	return m.st.LastSeenVersion
}

// dismissReleaseNotes closes the release notes overlay and persists the
// version so the dialog won't be shown again on subsequent startups.
func (m *model) dismissReleaseNotes() {
	if m.st != nil {
		m.st.LastSeenVersion = m.releaseNotesVersion
		_ = setLastSeenVersionRemote(m.releaseNotesVersion)
	}
	m.releaseNotesVersion = ""
	m.releaseNotesBody = ""
}

// viewReleaseNotes renders the release notes dialog centered in the
// terminal. The body is GitHub-flavoured markdown shown as-is (wrapped to
// the dialog width); long notes are truncated to fit the terminal height.
func (m model) viewReleaseNotes() string {
	// Size the box to the terminal, within sensible bounds.
	boxWidth := 72
	if m.width > 0 && boxWidth > m.width-4 {
		boxWidth = m.width - 4
	}
	boxWidth = max(boxWidth, 20)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("170")).
		Padding(1, 2).
		Width(boxWidth)

	// Inner content width = box width minus border (2) and padding (2*2).
	contentWidth := max(boxWidth-6, 1)

	title := dialogTitle.Render("✨ Fleet updated to " + m.releaseNotesVersion)

	body := strings.TrimSpace(m.releaseNotesBody)
	bodyStyle := dialogLabel.Width(contentWidth)

	// Cap the rendered body so the dialog never exceeds the terminal
	// height. Reserve rows for the border (2), padding (2), title (2),
	// hint (2) and a little breathing room.
	rendered := bodyStyle.Render(body)
	if m.height > 0 {
		maxBodyLines := max(m.height-10, 3)
		lines := strings.Split(rendered, "\n")
		if len(lines) > maxBodyLines {
			lines = append(lines[:maxBodyLines], dialogHint.Render("… (truncated)"))
			rendered = strings.Join(lines, "\n")
		}
	}

	hint := dialogHint.Render("Press any key to dismiss")

	content := title + "\n\n" + rendered + "\n" + hint

	width, height := m.width, m.height
	if width <= 0 {
		width = boxWidth + 4
	}
	if height <= 0 {
		height = lipgloss.Height(box.Render(content))
	}

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box.Render(content))
}
