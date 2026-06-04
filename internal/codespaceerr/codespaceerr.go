// Package codespaceerr holds the error-message prefixes the codespaces backend
// stamps onto provisioning failures, split out into a leaf package with no
// dependencies so BOTH the server-only codespaces backend and the thin TUI
// client can share them. The TUI matches an instance's persisted Error against
// these prefixes to route a failed codespace create to the right recovery
// dialog (auth scope / machine selection / limit reached) without importing the
// server-only backend package.
package codespaceerr

const (
	// AuthScope prefixes errors raised when the gh auth token is missing the
	// "codespace" OAuth scope.
	AuthScope = "codespaces:auth_scope:"
	// Limit prefixes errors raised when the user has reached their codespace
	// limit.
	Limit = "codespaces:limit:"
	// Machine prefixes errors raised when gh needs an interactive machine-type
	// selection but has no terminal.
	Machine = "codespaces:machine:"
)
