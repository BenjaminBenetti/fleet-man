package tui

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
)

type gitHostKeyMsg struct {
	ctx    context.Context
	key    *fleetgrpc.UnknownSSHHostKey
	answer chan string
	gen    int
}

type gitHostKeyClosedMsg struct{ prompt *gitHostKeyMsg }

func promptGitHostKey(program *tea.Program, gen int) func(context.Context, *fleetgrpc.UnknownSSHHostKey) string {
	return func(ctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		msg := &gitHostKeyMsg{ctx: ctx, key: key, answer: make(chan string, 1), gen: gen}
		program.Send(msg)
		defer program.Send(gitHostKeyClosedMsg{prompt: msg})
		select {
		case line := <-msg.answer:
			return line
		case <-ctx.Done():
			return ""
		}
	}
}

func (m *model) showNextGitHostKey() {
	for m.hostKeyPrompt == nil && len(m.gitHostKeyQueue) > 0 {
		msg := m.gitHostKeyQueue[0]
		m.gitHostKeyQueue = m.gitHostKeyQueue[1:]
		if msg.ctx.Err() != nil || msg.gen != m.watchGen {
			select {
			case msg.answer <- "":
			default:
			}
			continue
		}
		m.hostKeyPrompt = &hostKeyPrompt{url: msg.key.GetUrl(), key: msg.key, git: msg}
	}
}

func (m *model) resolveGitHostKey(key string) tea.Cmd {
	p := m.hostKeyPrompt
	line := ""
	switch key {
	case "a":
		if p.git.ctx.Err() == nil && p.git.gen == m.watchGen {
			line = p.key.GetKeys()[0].GetKnownHostsLine()
		}
	case "r", "n", "esc":
	default:
		return nil
	}
	select {
	case p.git.answer <- line:
	default:
	}
	m.hostKeyPrompt = nil
	if line != "" {
		m.message = "Saving the git host key on the fleet host and retrying the clone…"
	} else {
		m.message = "Git host key rejected — clone cancelled"
	}
	m.showNextGitHostKey()
	return nil
}
