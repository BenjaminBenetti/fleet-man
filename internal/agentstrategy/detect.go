package agentstrategy

import (
	"path"
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

// ToolForCommand names the agent a launch command runs: the first unquoted
// shell word whose executable name is a known agent CLI — `claude ...`,
// `IS_SANDBOX=1 claude ...`, `cd app && ~/.local/bin/claude ...`, or a package
// spec like `npx @anthropic-ai/claude-code@latest ...`. Quoted words
// are arguments (a prompt, a flag value), so an agent named inside one is not
// mistaken for the command. ok is false when no agent is recognized (a wrapper
// script, say).
func ToolForCommand(command string) (state.AgentTool, bool) {
	for _, w := range shellWords(command) {
		if w.quoted {
			continue
		}
		name := path.Base(w.text)
		if at := strings.LastIndex(name, "@"); at > 0 {
			name = name[:at] // a package spec's version
		}
		if tool, ok := agentBinaries[name]; ok {
			return tool, true
		}
	}
	return "", false
}

// shellWord is one word of a command line, and whether any of it was quoted
// or escaped.
type shellWord struct {
	text   string
	quoted bool
}

// shellWords splits a command line into words the way sh would for this
// purpose: whitespace and the control operators ;&|()<> separate words
// outside quotes; quotes and backslashes are removed. Expansions are left as
// written.
func shellWords(command string) []shellWord {
	var words []shellWord
	var cur strings.Builder
	quoted, inWord := false, false
	var quote rune // the open quote, or 0
	escaped := false
	flush := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), quoted: quoted})
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
		case strings.ContainsRune(" \t\n;&|()<>", r):
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}
