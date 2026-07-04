package coder

import (
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// coderMaxNameLen is Coder's maximum workspace name length.
const coderMaxNameLen = 32

// WorkspaceNameFor composes the Coder workspace name for an instance from its
// name prefix — the fleet's CoderWorkspaceName override, or the fleet name
// itself when unset — sanitized to Coder's naming rules. This is the single
// definition of the "<prefix>-<instance>" naming scheme shared by workspace
// creation (buildCoderBackend) and the ${INSTANCE_NAME} parameter substitution.
func WorkspaceNameFor(prefix, instance string) string {
	return sanitizeCoderName(prefix + "-" + instance)
}

// coderWorkspaceName derives a valid Coder workspace name from a workspace dir path.
// workspaceDir format: ~/.fleet/workspaces/{fleet}/{instance}/{fleet}
// Returns "{fleet}-{instance}" as the workspace name, sanitized for Coder.
// Only a FALLBACK: instances created since the workspace name became
// configurable (issue #221) may be named "<override>-<instance>", so callers
// prefer the recorded container ID (RegisterName / Up's name cache) and only
// land here for pre-existing backends with no registration.
func coderWorkspaceName(workspaceDir string) string {
	// Walk up the path to extract fleet and instance names
	// workspaceDir = .../workspaces/{fleet}/{instance}/{fleet}
	parent := filepath.Dir(workspaceDir) // .../workspaces/{fleet}/{instance}
	instance := filepath.Base(parent)    // {instance}
	grandparent := filepath.Dir(parent)  // .../workspaces/{fleet}
	fleet := filepath.Base(grandparent)  // {fleet}

	name := fleet + "-" + instance
	return sanitizeCoderName(name)
}

// sanitizeCoderName normalizes a name into a valid Coder workspace name:
// lowercase alphanumeric with single hyphens, no leading/trailing hyphen,
// max 32 characters. Falls back to "workspace" when the result is empty.
func sanitizeCoderName(name string) string {
	return backend.SanitizeName(name, coderMaxNameLen)
}
