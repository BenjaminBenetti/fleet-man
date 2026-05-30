package coder

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// templateListEntry mirrors the coder templates list JSON output.
type templateListEntry struct {
	Template struct {
		Name            string `json:"name"`
		ActiveVersionID string `json:"active_version_id"`
	} `json:"Template"`
}

// FetchActiveVersionID returns the active template version ID for a given template name.
func FetchActiveVersionID(templateName string) (string, error) {
	cmd := exec.Command("coder", "templates", "list", "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("coder templates list: %w", err)
	}

	var entries []templateListEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return "", fmt.Errorf("parsing templates list: %w", err)
	}

	for _, entry := range entries {
		if entry.Template.Name == templateName {
			return entry.Template.ActiveVersionID, nil
		}
	}

	return "", fmt.Errorf("template %q not found", templateName)
}
