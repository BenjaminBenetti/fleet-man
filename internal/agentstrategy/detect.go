package agentstrategy

import (
	"path"
	"regexp"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// agentBinaries maps an agent CLI's executable name to its tool.
var agentBinaries = map[string]state.AgentTool{
	"claude":      state.AgentToolClaude,
	"claude-code": state.AgentToolClaude, // npx/bunx @anthropic-ai/claude-code
	"codex":       state.AgentToolCodex,
	"gemini":      state.AgentToolGemini,
	"copilot":     state.AgentToolCopilot,
	"auggie":      state.AgentToolAuggie,
}

// launchers run the command that follows them (after their own flags and,
// for env, assignments), so the agent is the word after them.
var launchers = map[string]bool{
	"env": true, "exec": true, "command": true, "nohup": true, "time": true,
	"npx": true, "bunx": true, "pnpx": true,
}

// assignment matches a leading NAME=value word.
var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// ToolForCommand names the agent a launch command runs: the command word of
// one of its simple commands, past any VAR=value assignments and launchers —
// `claude ...`, `IS_SANDBOX=1 claude ...`, `cd app && ~/.local/bin/claude ...`,
// `npx -y @anthropic-ai/claude-code@latest ...`. Only command words count: an
// agent's name in an argument (a path like /src/claude-code, a prompt) is not
// the command. ok is false when no agent is recognized (a wrapper script, say).
func ToolForCommand(command string) (state.AgentTool, bool) {
	words := shellWords(command)
	for i, w := range words {
		if !w.start {
			continue
		}
		if tool, ok := commandTool(words[i:]); ok {
			return tool, true
		}
	}
	return "", false
}

// commandTool names the agent the simple command starting at words[0] runs.
func commandTool(words []shellWord) (state.AgentTool, bool) {
	launched := false
	for k, w := range words {
		if k > 0 && w.start {
			break // the next simple command
		}
		if w.quoted {
			return "", false // a quoted command word: not one fleet recognizes
		}
		name := path.Base(w.text)
		switch {
		case assignment.MatchString(w.text):
			continue
		case launched && strings.HasPrefix(w.text, "-"):
			continue // a launcher's flag
		case launchers[name]:
			launched = true
			continue
		}
		if at := strings.LastIndex(name, "@"); at > 0 {
			name = name[:at] // a package spec's version
		}
		tool, ok := agentBinaries[name]
		return tool, ok
	}
	return "", false
}

// shellWord is one word of a command line: whether any of it was quoted or
// escaped, and whether it starts a simple command (first word, or first after
// a control operator).
type shellWord struct {
	text   string
	quoted bool
	start  bool
}

// shellWords splits a command line into words the way sh would for this
// purpose: whitespace, redirections (<>) and the control operators ;&|() and
// newline separate words outside quotes, and a control operator starts a new
// simple command; quotes and backslashes are removed. Expansions are left as
// written.
func shellWords(command string) []shellWord {
	var words []shellWord
	var cur strings.Builder
	quoted, inWord := false, false
	atStart := true
	var quote rune // the open quote, or 0
	escaped := false
	flush := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), quoted: quoted, start: atStart})
			atStart = false
		}
		cur.Reset()
		quoted, inWord = false, false
	}
	for _, r := range command {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, quoted, inWord = true, true, true
		case r == '\'' || r == '"':
			quote, quoted, inWord = r, true, true
		case strings.ContainsRune(";&|()\n", r):
			flush()
			atStart = true
		case strings.ContainsRune(" \t<>", r):
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}
