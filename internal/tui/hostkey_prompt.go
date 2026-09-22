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
// surfaced once per key: concurrent dials of the same remote share the open
// prompt, and after a reject the background dials stay quiet until the user
// acts on that remote again — a connection test, enter on its settings row,
// an Armada switch to it, or enter on it in the Armada selector.
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

// forgetHostKeyRejections clears every remembered rejection for url: the user
// is acting on that remote again (an explicit switch), so its key may be asked
// about afresh rather than silently refused.
func (m *model) forgetHostKeyRejections(url string) {
	for k := range m.hostKeyDeclined {
		if strings.HasPrefix(k, url+"|") {
			delete(m.hostKeyDeclined, k)
		}
	}
}

// hasHostKeyRejection reports whether a rejection is on record for url.
func (m *model) hasHostKeyRejection(url string) bool {
	for k := range m.hostKeyDeclined {
		if strings.HasPrefix(k, url+"|") {
			return true
		}
	}
	return false
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
	if u := uk.GetUrl(); u != "" && u != url {
		// Stale: the detail is for another remote — a pre-switch Watch dial
		// whose error landed after FLEET_SSH already named the new one. Never
		// label one remote's key with another's name.
		return false
	}
	if p := m.hostKeyPrompt; p != nil {
		// One prompt at a time — but only the SAME remote's concurrent dials
		// share it. Another remote's error must take its normal failure path
		// (e.g. the add flow cancels with "Connection test failed"), or its
		// flow would sit waiting for a decision that is about a different host.
		return p.url == url
	}
	if origin == hostKeyOriginConnect && m.hostKeyDeclined[hostKeyDeclineKey(url, uk)] {
		return true // rejected earlier; background reconnects stay quiet
	}
	m.hostKeyPrompt = &hostKeyPrompt{url: url, key: uk, origin: origin}
	return true
}

// resolveHostKeyPrompt handles a keypress while the prompt is showing: accept
// (trust + continue) or reject (cancel). Only a deliberate `a` accepts —
// never enter or y: enter is the key that raised the prompt (it starts the
// connection test and the explicit ping), so a second press must not trust a
// key the user never read. Every other key is swallowed so it can't reach the
// page underneath; keys are ignored while the accept runs.
func (m *model) resolveHostKeyPrompt(key string) tea.Cmd {
	p := m.hostKeyPrompt
	if p == nil || p.busy {
		return nil
	}
	switch key {
	case "a":
		p.busy = true
		m.message = "Trusting host key and connecting to " + p.key.GetName() + "…"
		return trustHostKeyCmd(p.url, p.key.GetKeys()[0].GetKnownHostsLine(), p.origin)
	case "r", "n", "esc":
		m.hostKeyDeclined[hostKeyDeclineKey(p.url, p.key)] = true
		m.hostKeyPrompt = nil
		m.armadaStatus[p.url] = armadaStatus{state: armadaStatusError, err: "host key rejected"}
		// An add flow waiting on this remote is cancelled whichever path raised
		// the prompt (a Connect prompt for the FLEET_SSH remote can absorb that
		// remote's own add-flow test).
		if m.cancelArmadaAddFor(p.url) {
			m.message = "Host key rejected — " + p.key.GetName() + " was not added"
		} else {
			m.message = "Host key rejected — not connecting to " + p.key.GetName()
		}
		return nil
	}
	return nil
}

// armadaAddPendingFor reports whether the settings page's add flow is waiting
// on a connection test for url.
func (m *model) armadaAddPendingFor(url string) bool {
	sp, ok := m.currentPage.(*settingsPage)
	return ok && sp.armadaAddStage == armadaAddTesting && sp.armadaAddURL == url
}

// cancelArmadaAddFor cancels an add flow waiting on url, reporting whether
// there was one.
func (m *model) cancelArmadaAddFor(url string) bool {
	if !m.armadaAddPendingFor(url) {
		return false
	}
	m.currentPage.(*settingsPage).cancelArmadaAdd()
	return true
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
	if msg.err != nil {
		if m.offerHostKey(msg.url, msg.err, msg.origin) {
			return nil
		}
		// msg.err covers the whole accept — the known_hosts write (or a daemon
		// that restarted and no longer holds the offer) as much as the connect —
		// so say neither succeeded.
		m.armadaStatus[msg.url] = armadaStatus{state: armadaStatusError, err: armadaPingErrText(msg.url, msg.err)}
		m.cancelArmadaAddFor(msg.url)
		m.message = "Couldn't trust the host key and connect: " + armadaPingErrText(msg.url, msg.err)
		return nil
	}
	m.armadaStatus[msg.url] = armadaStatus{state: armadaStatusConnected}
	// The key is trusted now, so an earlier rejection for this remote no longer
	// applies (rejections are keyed by fingerprint; a future different key is
	// asked about afresh anyway). Leaving it would make the selector's
	// mid-ping retry gate fire on a healthy connection.
	m.forgetHostKeyRejections(msg.url)
	// What to resume is decided by the URL, not by which path raised the
	// prompt: an add flow waiting on this remote is finished (the accept
	// doubled as its connection test — the daemon re-resolved through the
	// trusted tunnel), and the connection this TUI is on is reconnected now
	// rather than waiting out the Watch stream's backoff. Both can apply at
	// once (adding the unregistered FLEET_SSH remote one is booted on).
	var cmds []tea.Cmd
	if m.armadaAddPendingFor(msg.url) {
		next := append(slices.Clone(m.armadaRemotes), configutil.ArmadaRemote{URL: msg.url})
		cmds = append(cmds, saveArmadaCmd(next, "added", -1))
	}
	m.message = "Host key trusted"
	if armadaCurrentKey() == msg.url {
		closeMutationConn()
		m.watchGen = bounceWatchStream()
		label := (armadaEntry{url: msg.url}).host()
		m.message = "Host key trusted — connecting to " + label + "…"
		cmds = append(cmds, switchReloadCmd(label, m.watchGen))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
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
	box := framedBox(activeTheme.Danger).Width(boxWidth)

	where := uk.GetHost()
	if uk.GetPort() != 0 {
		where = fmt.Sprintf("%s:%d", uk.GetHost(), uk.GetPort())
	}
	title := dialogTitle.Render("⚠ Unknown SSH host key for " + uk.GetName())
	intro := dialogHint.Render("fleet has never connected to " + where + ". Before accepting, compare the fingerprint " +
		"with the host's own: on the host, run ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub")
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
	hint := dialogLabel.Render("[a]ccept and connect   [r]eject (esc)")
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
