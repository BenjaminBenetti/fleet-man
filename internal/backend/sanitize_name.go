package backend

import (
	"regexp"
	"strings"
)

// invalidNameChars matches characters not allowed in a sanitized name:
// anything outside lowercase alphanumerics and hyphens.
var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]`)

// SanitizeName normalizes a name into a lowercase, hyphen-delimited
// identifier: lowercase alphanumerics with single hyphens, no leading or
// trailing hyphen, capped at maxLen characters. Falls back to "workspace"
// when the result is empty.
func SanitizeName(name string, maxLen int) string {
	name = strings.ToLower(name)
	name = invalidNameChars.ReplaceAllString(name, "-")
	// Collapse multiple hyphens.
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-")
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	name = strings.TrimRight(name, "-")
	if name == "" {
		name = "workspace"
	}
	return name
}
