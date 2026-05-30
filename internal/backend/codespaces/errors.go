package codespaces

import "strings"

// ErrPrefixAuthScope is the prefix used in error messages when the gh
// auth token is missing the "codespace" scope.
const ErrPrefixAuthScope = "codespaces:auth_scope:"

// ErrPrefixLimit is the prefix used in error messages when the user
// has reached their codespace limit.
const ErrPrefixLimit = "codespaces:limit:"

// ErrPrefixMachine is the prefix used in error messages when gh needs
// an interactive machine type selection but has no terminal.
const ErrPrefixMachine = "codespaces:machine:"

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
