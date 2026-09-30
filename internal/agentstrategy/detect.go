package agentstrategy

import (
	"path"
	"regexp"
	"slices"
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

// launcher describes a command that runs the command after it: which of its
// flags take the next word as their value, how many positional operands it
// takes before the command (timeout's duration), and whether it — or one of
// clearFlags — runs the command with the environment cleared.
type launcher struct {
	valueFlags []string
	positional int
	clears     bool
	clearFlags []string
}

// launchers are the wrappers fleet looks through to find the agent. sudo and
// doas (env_reset) and `env -i` drop the environment the launch exports, so an
// agent under them is recognized but cannot be handed the fleet MCP.
var launchers = map[string]launcher{
	"env":     {valueFlags: []string{"-u", "--unset", "-C", "--chdir"}, clearFlags: []string{"-i", "--ignore-environment", "-"}},
	"exec":    {valueFlags: []string{"-a"}, clearFlags: []string{"-c"}},
	"command": {},
	"nohup":   {},
	"time":    {valueFlags: []string{"-o", "--output", "-f", "--format"}},
	"nice":    {valueFlags: []string{"-n", "--adjustment"}},
	"timeout": {valueFlags: []string{"-s", "--signal", "-k", "--kill-after"}, positional: 1},
	"npx":     {valueFlags: []string{"-p", "--package"}},
	"bunx":    {valueFlags: []string{"-p", "--package"}},
	"pnpx":    {valueFlags: []string{"-p", "--package"}},
	"sudo":    {valueFlags: []string{"-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--close-from", "-D", "--chdir", "-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user"}, clears: true},
	"doas":    {valueFlags: []string{"-u", "-C"}, clears: true},
}

// assignment matches a leading NAME=value word.
var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// agentCommand is what fleet recognized a launch command runs.
type agentCommand struct {
	tool state.AgentTool
	// clearsEnv: a wrapper runs the agent with the environment cleared
	// (env -i, sudo, doas), so what the launch exports never reaches it.
	clearsEnv bool
}

// ToolForCommand names the agent a launch command runs: the command word of
// one of its simple commands, past any VAR=value assignments and launchers —
// `claude ...`, `IS_SANDBOX=1 claude ...`, `cd app && ~/.local/bin/claude ...`,
// `npx -y @anthropic-ai/claude-code@latest ...`, `timeout 2h claude ...`. Only
// command words count: an agent's name in an argument (a path like
// /src/claude-code, a prompt) is not the command. ok is false when no agent is
// recognized (a wrapper script, say).
func ToolForCommand(command string) (state.AgentTool, bool) {
	a, ok := detectAgent(command)
	return a.tool, ok
}

// detectAgent is ToolForCommand with what else fleet learned about the agent's
// simple command.
func detectAgent(command string) (agentCommand, bool) {
	words := shellWords(command)
	for i, w := range words {
		if !w.start {
			continue
		}
		if a, ok := commandAgent(words[i:]); ok {
			return a, true
		}
	}
	return agentCommand{}, false
}

// commandAgent recognizes the agent the simple command starting at words[0]
// runs: its first word that is not a VAR=value assignment, a launcher, or a
// launcher's flag, flag value or positional operand.
func commandAgent(words []shellWord) (agentCommand, bool) {
	var a agentCommand
	var outer *launcher // the innermost launcher so far
	skipValue := false  // the previous word was a flag taking this one
	positional := 0     // operands still owed to outer
	for k, w := range words {
		if k > 0 && w.start {
			break // the next simple command
		}
		if skipValue {
			skipValue = false
			continue
		}
		if assignment.MatchString(w.text) {
			continue // its value may well be quoted
		}
		if outer != nil && strings.HasPrefix(w.text, "-") {
			skipValue = slices.Contains(outer.valueFlags, w.text)
			if slices.Contains(outer.clearFlags, w.text) {
				a.clearsEnv = true
			}
			continue
		}
		if positional > 0 {
			positional-- // an operand, e.g. timeout's duration; it may be quoted
			continue
		}
		if w.quoted {
			return agentCommand{}, false // a quoted command word: not one fleet recognizes
		}
		name := path.Base(w.text)
		if l, ok := launchers[name]; ok {
			outer, positional = &l, l.positional
			a.clearsEnv = a.clearsEnv || l.clears
			continue
		}
		if at := strings.LastIndex(name, "@"); at > 0 {
			name = name[:at] // a package spec's version
		}
		tool, ok := agentBinaries[name]
		a.tool = tool
		return a, ok
	}
	return agentCommand{}, false
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
// purpose: whitespace and the control operators ;&|() and newline separate
// words outside quotes, and a control operator starts a new simple command.
// Redirections are dropped with their target and any fd number (2>/dev/null,
// >&2), so they are never taken for a command word. Quotes and backslashes are
// removed; expansions are left as written.
func shellWords(command string) []shellWord {
	var words []shellWord
	var cur strings.Builder
	quoted, inWord := false, false
	atStart := true
	var quote rune // the open quote, or 0
	escaped := false
	redirTarget := false // the next word is a redirection's target
	afterRedir := false  // the previous rune was < or >
	flush := func() {
		switch {
		case inWord && redirTarget:
			redirTarget = false // a redirection's target: not a word
		case inWord:
			words = append(words, shellWord{text: cur.String(), quoted: quoted, start: atStart})
			atStart = false
		}
		cur.Reset()
		quoted, inWord = false, false
	}
	for _, r := range command {
		wasRedir := afterRedir
		afterRedir = false
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
		case r == '<' || r == '>':
			if inWord && !quoted && isDigits(cur.String()) {
				cur.Reset() // the fd number of 2>..., not a word
				inWord = false
			}
			flush()
			redirTarget, afterRedir = true, true
		case r == '&' && wasRedir:
			// >&2: part of the redirection, not a control operator.
		case strings.ContainsRune(";&|()\n", r):
			flush()
			atStart = true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
