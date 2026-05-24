package devcontainer

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDevcontainer drops contents into <dir>/.devcontainer/devcontainer.json
// and fails the test if anything along the way errors.
func writeDevcontainer(t *testing.T, dir, contents string) {
	t.Helper()
	configDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "devcontainer.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}
}

// TestLoadFleetCustomizationsMissingConfigIsZero verifies that a
// workspace without a devcontainer.json yields the zero value and no
// error, so callers can safely treat the result as "use defaults".
func TestLoadFleetCustomizationsMissingConfigIsZero(t *testing.T) {
	fc, err := LoadFleetCustomizations(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFleetCustomizations on missing config = %v, want nil", err)
	}
	if fc.Browser.InitialURL != "" {
		t.Errorf("InitialURL = %q, want empty", fc.Browser.InitialURL)
	}
}

// TestLoadFleetCustomizationsInitialURL verifies the initialUrl is read
// out of the customizations.fleet.browser block.
func TestLoadFleetCustomizationsInitialURL(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"image": "mcr.microsoft.com/devcontainers/base:ubuntu",
		"customizations": {
			"fleet": {
				"browser": { "initialUrl": "http://localhost:3000" }
			}
		}
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}
	if got, want := fc.Browser.InitialURL, "http://localhost:3000"; got != want {
		t.Errorf("InitialURL = %q, want %q", got, want)
	}
}

// TestLoadFleetCustomizationsJSONC verifies the loader tolerates the
// comments and trailing commas allowed in devcontainer.json (JSONC) and
// ignores sibling tool namespaces under customizations.
func TestLoadFleetCustomizationsJSONC(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		// project dev container
		"image": "ubuntu",
		"customizations": {
			"vscode": { "extensions": ["golang.go"] },
			"fleet": {
				"browser": {
					"initialUrl": "https://example.test/app", // landing page
				},
			},
		},
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}
	if got, want := fc.Browser.InitialURL, "https://example.test/app"; got != want {
		t.Errorf("InitialURL = %q, want %q", got, want)
	}
}

// TestLoadFleetCustomizationsNoFleetBlock verifies that a devcontainer
// with customizations but no fleet namespace yields the zero value.
func TestLoadFleetCustomizationsNoFleetBlock(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"customizations": { "coder": { "ignore": true } }
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}
	if fc.Browser.InitialURL != "" {
		t.Errorf("InitialURL = %q, want empty", fc.Browser.InitialURL)
	}
}

// TestLoadFleetCustomizationsMalformedErrors verifies that genuinely
// invalid JSON surfaces as an error (callers may still choose to ignore
// it and fall back to defaults).
func TestLoadFleetCustomizationsMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{ "customizations": { "fleet": { `)

	if _, err := LoadFleetCustomizations(dir); err == nil {
		t.Fatal("LoadFleetCustomizations on malformed JSON = nil, want error")
	}
}
