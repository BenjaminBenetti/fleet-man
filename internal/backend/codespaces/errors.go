package codespaces

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/codespaceerr"
)

// The error-message prefixes now live in the dependency-free
// internal/codespaceerr leaf package so the thin TUI client can match against
// them without importing this server-only backend. They are re-exported here as
// aliases to keep this package's existing call sites unchanged.
const (
	// ErrPrefixAuthScope is the prefix used in error messages when the gh
	// auth token is missing the "codespace" scope.
	ErrPrefixAuthScope = codespaceerr.AuthScope
	// ErrPrefixLimit is the prefix used in error messages when the user
	// has reached their codespace limit.
	ErrPrefixLimit = codespaceerr.Limit
	// ErrPrefixMachine is the prefix used in error messages when gh needs
	// an interactive machine type selection but has no terminal.
	ErrPrefixMachine = codespaceerr.Machine
)

// isAuthScopeError returns true if the stderr output from gh indicates
// an authentication problem — either not logged in or missing the
// "codespace" OAuth scope.
func isAuthScopeError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "gh auth login") ||
		strings.Contains(lower, "gh auth refresh") ||
		strings.Contains(lower, "codespace") && strings.Contains(lower, "scope") ||
		strings.Contains(lower, "http 403") && strings.Contains(lower, "scope")
}

// isMachineSelectionError returns true if the stderr output from gh
// indicates it tried to prompt for machine type but had no terminal.
func isMachineSelectionError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no terminal") ||
		strings.Contains(lower, "machine type") && strings.Contains(lower, "error")
}

// isCodespaceLimitError returns true if the stderr output from gh
// indicates the user has reached their maximum codespace count.
func isCodespaceLimitError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "maximum number") ||
		strings.Contains(lower, "limit") && strings.Contains(lower, "codespace") ||
		strings.Contains(lower, "you have already reached") ||
		strings.Contains(lower, "out of codespaces")
}
