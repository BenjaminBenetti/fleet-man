package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// hostkey_prompt.go is the TUI half of the ssh host-key prompt. The local
// daemon never trusts an unknown host on its own: ssh runs with strict
// host-key checking, and a refusal comes back from ResolveArmadaRemote as a
// FailedPrecondition carrying an UnknownSSHHostKey detail (the host's keys as
// ssh-keyscan saw them, fingerprinted by ssh-keygen). Every path that dials an
// ssh:// remote — the add-remote connection test, an explicit ping, a switch,
// the Watch stream at boot or on reconnect — hands its error to offerHostKey,
// which turns that detail into ONE model-level overlay (the same treatment as
// the delegated-copy confirmation): host, key type, SHA256 fingerprint, and an
// accept / reject choice. Accept calls TrustSSHHostKey (the daemon appends the
// exact offered line and re-resolves) and then carries on with whatever the
// caller was doing; reject cancels it with a status message. The prompt is
// surfaced once per key: concurrent dials share the open prompt, and after a
// reject the background dials stay quiet until the user acts on that remote
// again (a connection test, enter on its settings row).
//
// A CHANGED key never reaches here: the daemon reports it as a plain error
// naming the offending known_hosts line, so it shows as an ordinary failure.

// hostKeyOrigin is what the user was doing when the key came up; it decides
// what accept continues and what reject cancels.
type hostKeyOrigin int

const (
	hostKeyOriginConnect hostKeyOrigin = iota // a dial of the current connection (boot, switch, Watch reconnect)
	hostKeyOriginAdd                          // the "+ Remote Fleet" connection test
	hostKeyOriginPing                         // an explicit ping (enter on a settings row)
)

// hostKeyPrompt is the pending decision.
type hostKeyPrompt struct {
	url    string
	key    *fleetgrpc.UnknownSSHHostKey
	origin hostKeyOrigin
	busy   bool // TrustSSHHostKey in flight
}

// hostKeyTrustedMsg delivers the accept's outcome.
type hostKeyTrustedMsg struct {
	url    string
	origin hostKeyOrigin
	err    error
}

// hostKeyDeclineKey identifies a decision by remote AND key, so a host that
// later presents a different key is asked about afresh.
func hostKeyDeclineKey(url string, uk *fleetgrpc.UnknownSSHHostKey) string {
	fp := ""
	if keys := uk.GetKeys(); len(keys) > 0 {
		fp = keys[0].GetFingerprint()
	}
	return url + "|" + fp
}

// hostKeyPromptShowing reports whether the overlay is up.
func (m *model) hostKeyPromptShowing() bool { return m.hostKeyPrompt != nil }

// offerHostKey opens the prompt when err carries an unknown-host-key detail.
// It returns true when the error has been taken over by the prompt (or by one
// already showing, or a standing rejection) — the caller must then not report
// it as a plain failure — and false for any other error.
func (m *model) offerHostKey(url string, err error, origin hostKeyOrigin) bool {
	uk := fleetclient.UnknownSSHHostKey(err)
	if uk == nil || len(uk.GetKeys()) == 0 {
		return false
	}
	if m.hostKeyPrompt != nil {
		return true // one prompt at a time; a concurrent dial's copy is swallowed
	}
	if origin == hostKeyOriginConnect && m.hostKeyDeclined[hostKeyDeclineKey(url, uk)] {
		return true // rejected earlier; background reconnects stay quiet
	}
	m.hostKeyPrompt = &hostKeyPrompt{url: url, key: uk, origin: origin}
	return true
}

// resolveHostKeyPrompt handles a keypress while the prompt is showing: accept
// (trust + continue) or reject (cancel). Every other key is swallowed so it
// can't reach the page underneath; keys are ignored while the accept runs.
func (m *model) resolveHostKeyPrompt(key string) tea.Cmd {
	p := m.hostKeyPrompt
	if p == nil || p.busy {
		return nil
	}
	switch key {
	case "a", "y", "enter":
		p.busy = true
		m.message = "Trusting host key and connecting to " + p.key.GetName() + "…"
		return trustHostKeyCmd(p.url, p.key.GetKeys()[0].GetKnownHostsLine(), p.origin)
	case "r", "d", "n", "esc":
		m.hostKeyDeclined[hostKeyDeclineKey(p.url, p.key)] = true
		m.hostKeyPrompt = nil
		m.armadaStatus[p.url] = armadaStatus{state: armadaStatusError, err: "host key rejected"}
		if p.origin == hostKeyOriginAdd {
			if sp, ok := m.currentPage.(*settingsPage); ok {
				sp.cancelArmadaAdd()
			}
			m.message = "Host key rejected — " + p.key.GetName() + " was not added"
		} else {
			m.message = "Host key rejected — not connecting to " + p.key.GetName()
		}
		return nil
	}
	return nil
}

// trustHostKeyCmd runs the accept against the local daemon.
func trustHostKeyCmd(url, line string, origin hostKeyOrigin) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), armadaSSHTimeout)
		defer cancel()
		return hostKeyTrustedMsg{url: url, origin: origin, err: fleetclient.TrustSSHHostKey(ctx, url, line)}
	}
}

// handleHostKeyTrusted finishes the accept: on success it resumes what the
// user was doing (register the remote, reconnect the current connection, or
// just mark the row connected); on failure it reports — or, if the daemon now
// reports another unknown key, asks again.
func (m *model) handleHostKeyTrusted(msg hostKeyTrustedMsg) tea.Cmd {
	m.hostKeyPrompt = nil
	settingsPage, _ := m.currentPage.(*settingsPage)
	if msg.err != nil {
		if m.offerHostKey(msg.url, msg.err, msg.origin) {
			return nil
		}
		m.armadaStatus[msg.url] = armadaStatus{state: armadaStatusError, err: armadaPingErrText(msg.url, msg.err)}
		if msg.origin == hostKeyOriginAdd && settingsPage != nil {
			settingsPage.cancelArmadaAdd()
		}
		m.message = "Host key trusted, but connecting failed: " + armadaPingErrText(msg.url, msg.err)
		return nil
	}
	m.armadaStatus[msg.url] = armadaStatus{state: armadaStatusConnected}
	switch msg.origin {
	case hostKeyOriginAdd:
		// The accept doubled as the connection test (the daemon re-resolved
		// through the trusted tunnel), so register the remote now — unless the
		// flow was abandoned while the prompt was up.
		if settingsPage != nil && settingsPage.armadaAddStage == armadaAddTesting && settingsPage.armadaAddURL == msg.url {
			next := append(slices.Clone(m.armadaRemotes), configutil.ArmadaRemote{URL: msg.url})
			return saveArmadaCmd(next, "added", -1)
		}
		m.message = "Host key trusted"
		return nil
	case hostKeyOriginConnect:
		if armadaCurrentKey() == msg.url {
			// Reconnect now rather than wait out the Watch stream's backoff.
			closeMutationConn()
			m.watchGen = bounceWatchStream()
			label := (armadaEntry{url: msg.url}).host()
			m.message = "Host key trusted — connecting to " + label + "…"
			return switchReloadCmd(label, m.watchGen)
		}
	}
	m.message = "Host key trusted"
	return nil
}

// viewHostKeyPrompt renders the centered overlay.
func (m model) viewHostKeyPrompt() string {
	p := m.hostKeyPrompt
	uk := p.key
	keys := uk.GetKeys()
	primary := keys[0]

	boxWidth := 78
	if m.width > 0 && boxWidth > m.width-4 {
		boxWidth = m.width - 4
	}
	boxWidth = max(boxWidth, 30)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("203")).
		Padding(1, 2).
		Width(boxWidth)

	where := uk.GetHost()
	if uk.GetPort() != 0 {
		where = fmt.Sprintf("%s:%d", uk.GetHost(), uk.GetPort())
	}
	title := dialogTitle.Render("⚠ Unknown SSH host key for " + uk.GetName())
	intro := dialogHint.Render("fleet has never connected to " + where + ". Compare the fingerprint with the host's own\n" +
		"(on the host: ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub) before accepting.")
	var rows strings.Builder
	rows.WriteString(dialogLabel.Render(fmt.Sprintf("%-11s %s", "Remote", p.url)) + "\n")
	rows.WriteString(dialogLabel.Render(fmt.Sprintf("%-11s %s", "Key type", primary.GetKeyType())) + "\n")
	rows.WriteString(selectedStyle.Render(fmt.Sprintf("%-11s %s", "Fingerprint", primary.GetFingerprint())) + "\n")
	rows.WriteString(dialogLabel.Render(fmt.Sprintf("%-11s %s", "Adds to", uk.GetKnownHostsPath())))
	if len(keys) > 1 {
		var others []string
		for _, k := range keys[1:] {
			others = append(others, k.GetKeyType()+" "+k.GetFingerprint())
		}
		rows.WriteString("\n" + dialogHint.Render("also offered (not added): "+strings.Join(others, ", ")))
	}
	hint := dialogLabel.Render("[a]ccept and connect   [r]eject")
	if p.busy {
		hint = m.spinner.View() + " trusting and connecting…"
	}
	content := title + "\n\n" + intro + "\n\n" + rows.String() + "\n\n" + hint
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(content))
}

// currentSSHURL is the ssh:// URL of the live connection, or "".
func currentSSHURL() string {
	return os.Getenv(fleetclient.EnvSSH)
}
