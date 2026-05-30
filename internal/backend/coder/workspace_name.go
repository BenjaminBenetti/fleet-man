package coder

import (
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// coderMaxNameLen is Coder's maximum workspace name length.
const coderMaxNameLen = 32

// coderWorkspaceName derives a valid Coder workspace name from a workspace dir path.
// workspaceDir format: ~/.fleet/workspaces/{fleet}/{instance}/{fleet}
// Returns "{fleet}-{instance}" as the workspace name, sanitized for Coder.
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
