package devcontainer

import (
	"os"
	"path/filepath"
	"strings"
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

// TestLoadFleetCustomizationsFleetLaunchSites verifies the fleetLaunch
// sites list is read out of the customizations.fleet block, including
// the subTitle and healthCheck fields.
func TestLoadFleetCustomizationsFleetLaunchSites(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"customizations": {
			"fleet": {
				"fleetLaunch": {
					"sites": [
						{
							"title": "API",
							"subTitle": "REST backend",
							"url": "http://localhost:3000",
							"healthCheck": "http://localhost:3000/healthz"
						},
						{
							"title": "Docs",
							"url": "http://localhost:8080"
						}
					]
				}
			}
		}
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}

	sites := fc.FleetLaunch.Sites
	if got, want := len(sites), 2; got != want {
		t.Fatalf("len(Sites) = %d, want %d", got, want)
	}

	first := sites[0]
	if first.Title != "API" {
		t.Errorf("Sites[0].Title = %q, want %q", first.Title, "API")
	}
	if first.SubTitle != "REST backend" {
		t.Errorf("Sites[0].SubTitle = %q, want %q", first.SubTitle, "REST backend")
	}
	if first.URL != "http://localhost:3000" {
		t.Errorf("Sites[0].URL = %q, want %q", first.URL, "http://localhost:3000")
	}
	if first.HealthCheck != "http://localhost:3000/healthz" {
		t.Errorf("Sites[0].HealthCheck = %q, want %q", first.HealthCheck, "http://localhost:3000/healthz")
	}

	// Optional fields left unset surface as the zero value.
	if second := sites[1]; second.SubTitle != "" || second.HealthCheck != "" {
		t.Errorf("Sites[1] optional fields = %q/%q, want empty", second.SubTitle, second.HealthCheck)
	}
}

// TestLoadFleetCustomizationsFleetLaunchApps verifies the fleetLaunch
// apps list is read out of the customizations.fleet block, including
// the command and port fields, and that Configured reflects an
// apps-only config.
func TestLoadFleetCustomizationsFleetLaunchApps(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"customizations": {
			"fleet": {
				"fleetLaunch": {
					"apps": [
						{
							"title": "Logs",
							"command": "docker run -d -p 16768:8080 amir20/dozzle:latest",
							"port": 16768
						},
						{
							"title": "Already up"
						}
					]
				}
			}
		}
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}

	apps := fc.FleetLaunch.Apps
	if got, want := len(apps), 2; got != want {
		t.Fatalf("len(Apps) = %d, want %d", got, want)
	}

	first := apps[0]
	if first.Title != "Logs" {
		t.Errorf("Apps[0].Title = %q, want %q", first.Title, "Logs")
	}
	if first.Command != "docker run -d -p 16768:8080 amir20/dozzle:latest" {
		t.Errorf("Apps[0].Command = %q, want the dozzle run command", first.Command)
	}
	if first.Port != 16768 {
		t.Errorf("Apps[0].Port = %d, want %d", first.Port, 16768)
	}

	// Optional fields left unset surface as the zero value.
	if second := apps[1]; second.Command != "" || second.Port != 0 {
		t.Errorf("Apps[1] optional fields = %q/%d, want empty/0", second.Command, second.Port)
	}

	// A Fleet Launch with only apps (no sites) is still configured.
	if !fc.FleetLaunch.Configured() {
		t.Error("Configured() = false for an apps-only Fleet Launch, want true")
	}
}

// TestLoadFleetCustomizationsNoFleetLaunchIsZero verifies a fleet block
// without a fleetLaunch yields an empty sites list rather than an error.
func TestLoadFleetCustomizationsNoFleetLaunchIsZero(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"customizations": {
			"fleet": { "browser": { "initialUrl": "http://localhost:3000" } }
		}
	}`)

	fc, err := LoadFleetCustomizations(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizations = %v, want nil", err)
	}
	if got := len(fc.FleetLaunch.Sites); got != 0 {
		t.Errorf("len(Sites) = %d, want 0", got)
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

// TestLoadFleetCustomizationsUpwardInStartDir verifies the upward search
// finds a config in the start directory itself, without climbing.
func TestLoadFleetCustomizationsUpwardInStartDir(t *testing.T) {
	dir := t.TempDir()
	writeDevcontainer(t, dir, `{
		"customizations": {
			"fleet": { "browser": { "initialUrl": "http://localhost:3000" } }
		}
	}`)

	fc, path, err := LoadFleetCustomizationsUpward(dir)
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsUpward = %v, want nil", err)
	}
	if want := filepath.Join(dir, ".devcontainer", "devcontainer.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got, want := fc.Browser.InitialURL, "http://localhost:3000"; got != want {
		t.Errorf("InitialURL = %q, want %q", got, want)
	}
}

// TestLoadFleetCustomizationsUpwardClimbs verifies the search walks up
// through intermediate directories (with no devcontainer.json of their
// own) to the project root's config — the `fleet launch`-from-a-subdir
// case the function exists for.
func TestLoadFleetCustomizationsUpwardClimbs(t *testing.T) {
	root := t.TempDir()
	writeDevcontainer(t, root, `{
		"customizations": {
			"fleet": {
				"fleetLaunch": { "sites": [ { "title": "API", "url": "http://localhost:3000" } ] }
			}
		}
	}`)

	deep := filepath.Join(root, "src", "internal", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep tree: %v", err)
	}

	fc, path, err := LoadFleetCustomizationsUpward(deep)
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsUpward = %v, want nil", err)
	}
	if want := filepath.Join(root, ".devcontainer", "devcontainer.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got := len(fc.FleetLaunch.Sites); got != 1 {
		t.Fatalf("len(Sites) = %d, want 1", got)
	}
}

// TestLoadFleetCustomizationsUpwardSkipsFleetlessConfig verifies that a
// devcontainer.json WITHOUT a customizations.fleet block does not stop
// the climb: the search continues to an ancestor that has one.
func TestLoadFleetCustomizationsUpwardSkipsFleetlessConfig(t *testing.T) {
	root := t.TempDir()
	writeDevcontainer(t, root, `{
		"customizations": {
			"fleet": { "browser": { "initialUrl": "http://localhost:9999" } }
		}
	}`)

	nested := filepath.Join(root, "nested-project")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writeDevcontainer(t, nested, `{
		"customizations": { "vscode": { "extensions": ["golang.go"] } }
	}`)

	fc, path, err := LoadFleetCustomizationsUpward(nested)
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsUpward = %v, want nil", err)
	}
	if want := filepath.Join(root, ".devcontainer", "devcontainer.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got, want := fc.Browser.InitialURL, "http://localhost:9999"; got != want {
		t.Errorf("InitialURL = %q, want %q", got, want)
	}
}

// TestLoadFleetCustomizationsUpwardEmptyFleetBlockStops verifies that an
// explicitly present (but empty) fleet block stops the climb: presence
// of the namespace, not configured content, is the stopping condition.
func TestLoadFleetCustomizationsUpwardEmptyFleetBlockStops(t *testing.T) {
	root := t.TempDir()
	writeDevcontainer(t, root, `{
		"customizations": {
			"fleet": { "browser": { "initialUrl": "http://localhost:9999" } }
		}
	}`)

	nested := filepath.Join(root, "nested-project")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writeDevcontainer(t, nested, `{
		"customizations": { "fleet": {} }
	}`)

	fc, path, err := LoadFleetCustomizationsUpward(nested)
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsUpward = %v, want nil", err)
	}
	if want := filepath.Join(nested, ".devcontainer", "devcontainer.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if fc.Browser.InitialURL != "" {
		t.Errorf("InitialURL = %q, want empty (nested empty fleet block wins)", fc.Browser.InitialURL)
	}
}

// TestLoadFleetCustomizationsUpwardNotFound verifies that reaching the
// filesystem root without finding a fleet-configured devcontainer.json
// yields the zero value, an empty path, and no error.
func TestLoadFleetCustomizationsUpwardNotFound(t *testing.T) {
	fc, path, err := LoadFleetCustomizationsUpward(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsUpward = %v, want nil", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
	if fc.Browser.InitialURL != "" || fc.FleetLaunch.Configured() {
		t.Errorf("customizations = %+v, want zero value", fc)
	}
}

// TestLoadFleetCustomizationsUpwardMalformedErrors verifies that a
// malformed devcontainer.json on the way up is an error naming the file,
// rather than being silently skipped in favour of an ancestor's config.
func TestLoadFleetCustomizationsUpwardMalformedErrors(t *testing.T) {
	root := t.TempDir()
	writeDevcontainer(t, root, `{
		"customizations": { "fleet": {} }
	}`)

	nested := filepath.Join(root, "nested-project")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writeDevcontainer(t, nested, `{ "customizations": { "fleet": { `)

	_, _, err := LoadFleetCustomizationsUpward(nested)
	if err == nil {
		t.Fatal("LoadFleetCustomizationsUpward over malformed JSON = nil, want error")
	}
	if want := filepath.Join(nested, ".devcontainer", "devcontainer.json"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the malformed file %q", err, want)
	}
}

// TestLoadFleetCustomizationsFromFile verifies the explicit-path loader
// parses a JSONC devcontainer.json (comments + trailing commas) with a
// fleetLaunch block of both sites and apps.
func TestLoadFleetCustomizationsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devcontainer.json")
	contents := `{
		// explicit-path config
		"image": "ubuntu",
		"customizations": {
			"fleet": {
				"fleetLaunch": {
					"sites": [
						{ "title": "API", "url": "http://localhost:3000" },
					],
					"apps": [
						{ "title": "Web", "command": "npm start", "port": 5173 },
					],
				},
			},
		},
	}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	fc, err := LoadFleetCustomizationsFromFile(path)
	if err != nil {
		t.Fatalf("LoadFleetCustomizationsFromFile = %v, want nil", err)
	}
	if got := len(fc.FleetLaunch.Sites); got != 1 {
		t.Fatalf("len(Sites) = %d, want 1", got)
	}
	if got := fc.FleetLaunch.Sites[0].URL; got != "http://localhost:3000" {
		t.Errorf("Sites[0].URL = %q, want http://localhost:3000", got)
	}
	if got := len(fc.FleetLaunch.Apps); got != 1 {
		t.Fatalf("len(Apps) = %d, want 1", got)
	}
	if got := fc.FleetLaunch.Apps[0].Port; got != 5173 {
		t.Errorf("Apps[0].Port = %d, want 5173", got)
	}
}

// TestLoadFleetCustomizationsFromFileMissing verifies a missing explicit
// path is an error (unlike the workspace loader, which treats absence as
// "use defaults").
func TestLoadFleetCustomizationsFromFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := LoadFleetCustomizationsFromFile(missing); err == nil {
		t.Fatal("LoadFleetCustomizationsFromFile on missing file = nil, want error")
	}
}
