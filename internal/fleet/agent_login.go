package fleet

import (
	"path"
	"regexp"
	"strings"
)

// agent_login.go names the agent tools the automation login check knows.
// Logins are per location (the daemon host, or one cluster): an agent fired
// where its tool was never logged in would sit at a login prompt until it is
// reaped. So the scheduler refuses a cluster agent's firing when its command
// starts one of these tools, nothing in the command or the fleet's
// devcontainer env sets the tool's API-key variable, and the location holds
// no login for it. Other commands are never checked.

// LoginTool is an agent tool whose runs need a login where they run.
type LoginTool struct {
	// Tool is the command that starts the tool ("claude"), and the name
	// status and the Log in action carry.
	Tool string
	// Name is how messages name it ("Claude").
	Name string
	// MountName is the name of the tool's mount (internal/mounts/resolver),
	// which the sync policy table's roots carry (syncpolicy.Root.Tool): the
	// folder its login file lives in.
	MountName string
	// KeyVars are the environment variables that sign the tool in without a
	// login (an API key, a long-lived token, a cloud provider).
	KeyVars []string
	// LoginCommand is what the Log in action runs in a session to log in.
	LoginCommand string
}

// loginTools are the tools the login check knows, by command.
var loginTools = []LoginTool{
	{
		Tool: "claude", Name: "Claude", MountName: "Claude Code",
		KeyVars:      []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"},
		LoginCommand: "claude",
	},
	{
		Tool: "codex", Name: "Codex", MountName: "Codex",
		KeyVars:      []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
		LoginCommand: "codex login",
	},
	{
		Tool: "auggie", Name: "Auggie", MountName: "Auggie",
		KeyVars:      []string{"AUGMENT_SESSION_AUTH", "AUGMENT_API_TOKEN"},
		LoginCommand: "auggie login",
	},
}

// LoginTools returns every tool the login check knows.
func LoginTools() []LoginTool {
	return append([]LoginTool(nil), loginTools...)
}

// LoginToolNamed returns the tool whose Tool is name.
func LoginToolNamed(name string) (LoginTool, bool) {
	for _, t := range loginTools {
		if t.Tool == name {
			return t, true
		}
	}
	return LoginTool{}, false
}

// AgentLoginTool returns the login tool an agent command starts: its first
// word after any leading variable assignments and an `env` or `exec`
// prefix, by base name (/usr/local/bin/claude is claude). ok is false for
// any other command, which the check leaves alone.
func AgentLoginTool(command string) (LoginTool, bool) {
	words := commandWords(command, 8)
	for len(words) > 0 {
		w := words[0]
		switch {
		case isAssignment(w):
			words = words[1:]
			continue
		case w == "env" || w == "exec" || w == "command" || w == "nohup":
			words = words[1:]
			// env's own options ("env -i", "env -u NAME") come before the
			// assignments and the command.
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if words[0] == "-u" && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
			continue
		}
		return LoginToolNamed(path.Base(w))
	}
	return LoginTool{}, false
}

// SetBy reports whether text (an agent command) assigns one of the tool's
// key variables: `ANTHROPIC_API_KEY=... claude`, `export OPENAI_API_KEY=...;
// codex ...`.
func (t LoginTool) SetBy(text string) bool {
	for _, v := range t.KeyVars {
		if regexp.MustCompile(`(^|[\s;&|(])(export\s+)?` + regexp.QuoteMeta(v) + `=`).MatchString(text) {
			return true
		}
	}
	return false
}

// SetIn reports whether names (an environment's variable names) holds one
// of the tool's key variables.
func (t LoginTool) SetIn(names map[string]bool) bool {
	for _, v := range t.KeyVars {
		if names[v] {
			return true
		}
	}
	return false
}

// isAssignment reports whether a shell word is a variable assignment
// (NAME=value).
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// commandWords splits the start of a shell command into at most max words,
// honouring single and double quotes and backslashes, and stopping at the
// first ; & | or newline outside quotes: enough to find the command a line
// starts, never a full shell parse.
func commandWords(command string, max int) []string {
	var (
		words []string
		cur   strings.Builder
		in    bool // a word is open
		quote rune
	)
	flush := func() {
		if in {
			words = append(words, cur.String())
			cur.Reset()
			in = false
		}
	}
	runes := []rune(command)
	for i := 0; i < len(runes) && len(words) < max; i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			in = true
		case r == ' ' || r == '\t':
			flush()
		case r == ';' || r == '&' || r == '|' || r == '\n':
			flush()
			return words
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	flush()
	return words
}
