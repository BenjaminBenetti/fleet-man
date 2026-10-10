// Package redact hides secret values in output fleet keeps or shows: the
// creation log a provisioning job writes, the events it streams to clients and
// the failure it records on the instance.
//
// What it hides today are environment values. The devcontainer CLI turns
// containerEnv (and any -e in runArgs) into `-e KEY=VALUE` on the docker run it
// logs, and repeats containerEnv and remoteEnv in the devcontainer.metadata
// label JSON on the same line; those values are often API keys. KEY stays, so
// a reader can still see which variables were set.
package redact

import (
	"strings"
	"unicode"
)

// Placeholder replaces a redacted value.
const Placeholder = "<redacted>"

// EnvArgs returns a copy of argv with the value of every docker environment
// flag replaced by Placeholder: "-e KEY=VALUE", "--env KEY=VALUE",
// "--env=KEY=VALUE" and "-eKEY=VALUE" keep KEY= and lose VALUE. A flag that
// only names a variable ("-e KEY", which docker fills in from its own
// environment) has no value to hide and is kept as it is. Every argument also
// goes through EnvObjects, for the devcontainer.metadata label. argv is not
// changed.
func EnvArgs(argv []string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = EnvObjects(arg)
	}
	for i := 0; i < len(out); i++ {
		arg := out[i]
		if isEnvFlag(arg) {
			if i+1 < len(out) {
				if redacted, ok := redactAssignment(out[i+1]); ok {
					out[i+1] = redacted
					i++
				}
			}
			continue
		}
		if redacted, ok := redactAttached(arg); ok {
			out[i] = redacted
		}
	}
	return out
}

// EnvLine is EnvArgs for one line of text that holds a space-joined argv,
// which is how the devcontainer CLI logs its docker commands
// ("Run: docker run ... -e KEY=VALUE ..."). Joining loses the word boundaries a
// value with spaces in it had, so the value is taken to run up to the next word
// that starts with "-" (the next flag): every word in between is dropped with
// it. The line also goes through EnvObjects. Text that is not an env value is
// kept byte for byte.
func EnvLine(line string) string {
	return envFlagsLine(EnvObjects(line))
}

// envFlagsLine hides the values of the env flags on line (see EnvLine).
func envFlagsLine(line string) string {
	if !strings.Contains(line, "-e") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	const (
		normal = iota
		wantValue
		inValue
	)
	mode := normal
	i := 0
	for i < len(line) {
		// Copy the whitespace before the next word, unless the word turns out
		// to be the rest of a redacted value (then both go).
		start := i
		for i < len(line) && isSpace(line[i]) {
			i++
		}
		gap := line[start:i]
		if i == len(line) {
			b.WriteString(gap)
			break
		}
		wordStart := i
		for i < len(line) && !isSpace(line[i]) {
			i++
		}
		word := line[wordStart:i]

		if mode == inValue {
			if !strings.HasPrefix(word, "-") {
				continue
			}
			mode = normal
		}
		b.WriteString(gap)
		if mode == wantValue {
			mode = normal
			if redacted, ok := redactAssignment(word); ok {
				b.WriteString(redacted)
				mode = inValue
				continue
			}
		}
		switch {
		case isEnvFlag(word):
			b.WriteString(word)
			mode = wantValue
		default:
			if redacted, ok := redactAttached(word); ok {
				b.WriteString(redacted)
				mode = inValue
				continue
			}
			b.WriteString(word)
		}
	}
	return b.String()
}

// EnvText applies EnvLine to every line of s.
func EnvText(s string) string {
	if !strings.Contains(s, "-e") && !strings.Contains(s, "Env") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = EnvLine(line)
	}
	return strings.Join(lines, "\n")
}

// isEnvFlag reports whether arg is a docker env flag whose value is the next
// argument.
func isEnvFlag(arg string) bool {
	return arg == "-e" || arg == "--env"
}

// redactAssignment hides the value of a KEY=VALUE argument. ok is false when
// arg assigns nothing (no "="), as with "-e KEY".
func redactAssignment(arg string) (string, bool) {
	key, _, ok := strings.Cut(arg, "=")
	if !ok {
		return arg, false
	}
	return key + "=" + Placeholder, true
}

// redactAttached hides the value of a flag that carries its assignment in the
// same argument: "--env=KEY=VALUE", or "-eKEY=VALUE" when KEY is a valid
// variable name (so other words that start with -e are left alone).
func redactAttached(arg string) (string, bool) {
	if rest, ok := strings.CutPrefix(arg, "--env="); ok {
		if redacted, ok := redactAssignment(rest); ok {
			return "--env=" + redacted, true
		}
		return arg, false
	}
	rest, ok := strings.CutPrefix(arg, "-e")
	if !ok {
		return arg, false
	}
	key, _, ok := strings.Cut(rest, "=")
	if !ok || !isEnvName(key) {
		return arg, false
	}
	return "-e" + key + "=" + Placeholder, true
}

// isEnvName reports whether s is a shell variable name.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r != '_' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t'
}
