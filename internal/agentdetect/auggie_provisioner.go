package agentdetect

import (
	"fmt"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
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
	home, err := p.resolveHome()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	scriptPath := path.Join(home, AuggieScriptSuffix)
	if err := p.dropHookScript(scriptPath); err != nil {
		return fmt.Errorf("install hook script: %w", err)
	}
	if err := p.injectSettings(home, scriptPath); err != nil {
		return fmt.Errorf("edit settings.json: %w", err)
	}
	return nil
}

// ===========================================
// Private helpers
// ===========================================

// settingsSuffix is the home-relative path of auggie's settings file.
const auggieSettingsSuffix = ".augment/settings.json"

// resolveHome asks the container for $HOME. The hook script and settings.json
// both live under it, and their absolute paths are baked into the in-container
// commands (and into settings.json's hook command field).
func (p *AuggieProvisioner) resolveHome() (string, error) {
	out, err := p.exec.Run([]string{"sh", "-c", "echo \"$HOME\""}, nil)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", fmt.Errorf("container reported empty $HOME")
	}
	return home, nil
}

// dropHookScript writes the embedded hook script bytes to scriptPath and sets
// the executable bit, via an inline (base64-in-argv) write rather than streaming
// over stdin — a stdin-reading `cat > file` hangs forever on the coder backend
// (issue #223). The write mkdir's the parent and is atomic.
func (p *AuggieProvisioner) dropHookScript(scriptPath string) error {
	argv, err := backend.InlineWriteScript(scriptPath, AuggieHookScript, 0o755)
	if err != nil {
		return err
	}
	_, err = p.exec.Run(argv, nil)
	return err
}

// injectSettings reads the current ~/.augment/settings.json (or treats
// it as absent), passes it through InjectAuggieHooks, and writes the
// result back atomically.
func (p *AuggieProvisioner) injectSettings(home, scriptPath string) error {
	settingsPath := path.Join(home, auggieSettingsSuffix)
	current, err := p.readSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	updated, err := InjectAuggieHooks(current, scriptPath)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	if err := p.writeSettings(settingsPath, updated); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readSettings cats settingsPath. A missing file produces empty bytes (treated
// by InjectAuggieHooks as "the file does not exist"), not an error. The read
// produces output and exits, so it has no stdin-EOF dependency.
func (p *AuggieProvisioner) readSettings(settingsPath string) ([]byte, error) {
	q := backend.ShellQuote(settingsPath)
	script := fmt.Sprintf(`if [ -f %s ]; then cat %s; fi`, q, q)
	return p.exec.Run([]string{"sh", "-c", script}, nil)
}

// writeSettings writes content to settingsPath atomically (same-directory tmp
// then rename, handled by the inline writer, which also mkdir's the parent).
// Inline (base64-in-argv) rather than stdin-streamed so it cannot hang on the
// coder backend (issue #223).
func (p *AuggieProvisioner) writeSettings(settingsPath string, content []byte) error {
	argv, err := backend.InlineWriteScript(settingsPath, content, 0o644)
	if err != nil {
		return err
	}
	_, err = p.exec.Run(argv, nil)
	return err
}
