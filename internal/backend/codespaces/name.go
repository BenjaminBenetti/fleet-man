package codespaces

import (
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// codespaceMaxNameLen is the maximum length for a codespace display name.
const codespaceMaxNameLen = 48

// codespaceName derives a display name for a GitHub Codespace from a workspace dir path.
// workspaceDir format: ~/.fleet/workspaces/{fleet}/{instance}/{fleet}
// Returns "{fleet}-{instance}" sanitized for Codespace display names.
func codespaceName(workspaceDir string) string {
	parent := filepath.Dir(workspaceDir)    // .../workspaces/{fleet}/{instance}
	instance := filepath.Base(parent)       // {instance}
	grandparent := filepath.Dir(parent)     // .../workspaces/{fleet}
	fleetName := filepath.Base(grandparent) // {fleet}

	name := fleetName + "-" + instance
	return backend.SanitizeName(name, codespaceMaxNameLen)
}
