// Package shellquote is the single implementation of POSIX-shell single-quote
// escaping. The same three-line idiom was previously copied into
// internal/backend, internal/dotfiles, internal/tui and internal/fleet —
// a quoting bug is shell injection, so the rule lives once, in a leaf package
// with no dependencies that every layer (backend code and depguard-restricted
// client code alike) may import.
package shellquote

import "strings"

// Single wraps value in single quotes with embedded single quotes escaped via
// the '\'' idiom (close the quote, emit an escaped quote, reopen the quote),
// making any value safe to drop into a /bin/sh command literal.
func Single(value string) string {
	return "'" + EscapeSingle(value) + "'"
}

// EscapeSingle rewrites the single quotes in s so it is safe to splice INSIDE
// an existing single-quoted shell string — the inner half of Single. Used when
// the surrounding quotes are written by a template the value is substituted
// into (e.g. an agent command's '${PROMPT}' placeholder).
func EscapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}
