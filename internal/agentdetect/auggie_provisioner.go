package agentdetect

import (
	"fmt"
	"path"
	"strings"
)

// ===========================================
// AuggieProvisioner
// ===========================================
//
// AuggieProvisioner is the Augment CLI counterpart to
// ClaudeProvisioner. It installs the two pieces of in-container state
// fleet-man needs to read auggie's run state from hooks:
//
//   1. The state-writing shell script at $HOME/AuggieScriptSuffix,
//      copied from the AuggieHookScript bytes embedded in the binary
//      and made executable.
//
//   2. Hook entries in ~/.augment/settings.json that point at that
//      script for every event we care about, merged into the user's
//      existing settings via InjectAuggieHooks.
//
// Idempotent and atomic with the same guarantees as ClaudeProvisioner:
// the script is rewritten verbatim, InjectAuggieHooks is idempotent on
// its own output, and settings.json is written via same-directory tmp
// + rename so a reader never sees a partial document.

// AuggieProvisioner orchestrates installation of the in-container
// auggie state-hook script and hook entries.
type AuggieProvisioner struct {
	exec ContainerExecutor
}

// ===========================================
// Constructors
// ===========================================

// NewAuggieProvisioner returns a provisioner that runs commands inside
// the target container via the given executor.
func NewAuggieProvisioner(exec ContainerExecutor) *AuggieProvisioner {
	return &AuggieProvisioner{exec: exec}
}

// ===========================================
// Public API
// ===========================================

// Provision drops the hook script and edits ~/.augment/settings.json.
// Errors are wrapped with phase context so a partial failure tells the
// caller which step did not complete; the caller may surface them as
// warnings rather than aborting container creation.
func (p *AuggieProvisioner) Provision() error {
	scriptPath, err := p.resolveScriptPath()
	if err != nil {
		return fmt.Errorf("resolve script path: %w", err)
	}
	if err := p.dropHookScript(scriptPath); err != nil {
		return fmt.Errorf("install hook script: %w", err)
	}
	if err := p.injectSettings(scriptPath); err != nil {
		return fmt.Errorf("edit settings.json: %w", err)
	}
	return nil
}

// ===========================================
// Private helpers
// ===========================================

// resolveScriptPath asks the container for $HOME and joins it with
// AuggieScriptSuffix to produce the absolute path the hook script will
// be dropped at and that settings.json will reference.
func (p *AuggieProvisioner) resolveScriptPath() (string, error) {
	out, err := p.exec.Run([]string{"sh", "-c", "echo \"$HOME\""}, nil)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", fmt.Errorf("container reported empty $HOME")
	}
	return path.Join(home, AuggieScriptSuffix), nil
}

// dropHookScript writes the embedded hook script bytes to scriptPath
// and sets the executable bit, mkdir-ing the parent first. All three
// operations are bundled into one shell invocation to minimise
// round-trips to the container exec layer.
func (p *AuggieProvisioner) dropHookScript(scriptPath string) error {
	parentDir := path.Dir(scriptPath)
	script := fmt.Sprintf(
		`mkdir -p %q && cat > %q && chmod +x %q`,
		parentDir, scriptPath, scriptPath,
	)
	_, err := p.exec.Run([]string{"sh", "-c", script}, AuggieHookScript)
	return err
}

// injectSettings reads the current ~/.augment/settings.json (or treats
// it as absent), passes it through InjectAuggieHooks, and writes the
// result back atomically.
func (p *AuggieProvisioner) injectSettings(scriptPath string) error {
	current, err := p.readSettings()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	updated, err := InjectAuggieHooks(current, scriptPath)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	if err := p.writeSettings(updated); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readSettings cats ~/.augment/settings.json. A missing file produces
// empty bytes (treated by InjectAuggieHooks as "the file does not
// exist"), not an error.
func (p *AuggieProvisioner) readSettings() ([]byte, error) {
	const script = `if [ -f "$HOME/.augment/settings.json" ]; then cat "$HOME/.augment/settings.json"; fi`
	return p.exec.Run([]string{"sh", "-c", script}, nil)
}

// writeSettings writes content to ~/.augment/settings.json atomically:
// same-directory tmp file then rename. mkdir of ~/.augment handles the
// first-time case where auggie has never been run in this container.
func (p *AuggieProvisioner) writeSettings(content []byte) error {
	const script = `mkdir -p "$HOME/.augment" && ` +
		`cat > "$HOME/.augment/settings.json.tmp" && ` +
		`mv "$HOME/.augment/settings.json.tmp" "$HOME/.augment/settings.json"`
	_, err := p.exec.Run([]string{"sh", "-c", script}, content)
	return err
}
