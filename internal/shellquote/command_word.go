package shellquote

import "strings"

// CommandWord quotes value for the command word of a line that may reach a
// shell other than /bin/sh. tmux runs a lone shell-command through the user's
// default-shell, and PowerShell, nushell and xonsh read a quoted word there as
// a string, not a program to run. So a value made only of characters no shell
// treats specially (letters, digits, _ - . / +) is returned as is, and anything
// else gets Single, which every POSIX shell and fish still run.
func CommandWord(value string) string {
	if value == "" {
		return Single(value)
	}
	for _, r := range value {
		if !isPlainWordRune(r) {
			return Single(value)
		}
	}
	return value
}

func isPlainWordRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("_-./+", r)
}
