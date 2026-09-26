package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	tea "github.com/charmbracelet/bubbletea"
)

func TestGitHostKeyPromptDecision(t *testing.T) {
	for _, decision := range []string{"a", "r", "esc"} {
		t.Run(decision, func(t *testing.T) {
			m := armadaTestModel(newSettingsPage())
			trusted := stubTrust(t, nil)
			key := fleetclient.UnknownSSHHostKey(unknownKeyErr(t, "git@server:repo.git", "SHA256:git-key"))
			msg := &gitHostKeyMsg{ctx: context.Background(), key: key, answer: make(chan string, 1), gen: m.watchGen}
			updated, _ := m.Update(msg)
			*m = updated.(model)
			view := m.viewHostKeyPrompt()
			for _, want := range []string{"SHA256:git-key", "git@server:repo.git", "connected fleet host", "retry clone"} {
				if !strings.Contains(view, want) {
					t.Errorf("prompt missing %q: %s", want, view)
				}
			}
			for _, accidental := range []string{"enter", "y", " "} {
				m.resolveHostKeyPrompt(accidental)
				select {
				case <-msg.answer:
					t.Fatalf("%q must not accept a key", accidental)
				default:
				}
			}
			m.resolveHostKeyPrompt(decision)
			want := ""
			if decision == "a" {
				want = key.GetKeys()[0].GetKnownHostsLine()
			}
			if got := <-msg.answer; got != want || m.hostKeyPromptShowing() {
				t.Fatalf("answer=%q want=%q prompt=%v", got, want, m.hostKeyPrompt)
			}
			if len(*trusted) != 0 {
				t.Fatal("git approval must not call the local Armada trust RPC")
			}
		})
	}
}

func TestGitHostKeyPromptQueueCancellationAndStaleConnection(t *testing.T) {
	m := armadaTestModel(newSettingsPage())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := fleetclient.UnknownSSHHostKey(unknownKeyErr(t, "git@server:repo.git", "SHA256:git-key"))
	first := &gitHostKeyMsg{ctx: ctx, key: key, answer: make(chan string, 1), gen: m.watchGen}
	second := &gitHostKeyMsg{ctx: context.Background(), key: key, answer: make(chan string, 1), gen: m.watchGen}
	m.gitHostKeyQueue = []*gitHostKeyMsg{first, second}
	m.showNextGitHostKey()
	if m.hostKeyPrompt.git != first {
		t.Fatal("first prompt not shown")
	}
	cancel()
	updated, _ := m.Update(gitHostKeyClosedMsg{prompt: first})
	*m = updated.(model)
	if m.hostKeyPrompt.git != second {
		t.Fatal("cancelled prompt did not yield to next request")
	}
	// A delayed accept after switching daemons must not trust on the old host.
	m.watchGen++
	m.resolveHostKeyPrompt("a")
	if line := <-second.answer; line != "" {
		t.Fatalf("stale connection accepted %q", line)
	}
	// Expired messages arriving after cancellation must not raise an overlay.
	updated, _ = m.Update(first)
	*m = updated.(model)
	if m.hostKeyPromptShowing() {
		t.Fatal("expired prompt shown")
	}
	// A background close from an earlier request cannot dismiss an Armada prompt.
	m.offerHostKey("ssh://desktop", unknownKeyErr(t, "ssh://desktop", "SHA256:armada"), hostKeyOriginPing)
	updated, _ = m.Update(gitHostKeyClosedMsg{prompt: second})
	*m = updated.(model)
	if !m.hostKeyPromptShowing() || m.hostKeyPrompt.git != nil {
		t.Fatal("git cancellation dismissed Armada prompt")
	}
	// Other keys stay swallowed by the shared overlay.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}
