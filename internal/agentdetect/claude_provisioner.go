package agentdetect

import (
	"fmt"
	"path"
	"strings"
)

// ===========================================
// ClaudeProvisioner
// ===========================================
//
// ClaudeProvisioner installs the two pieces of in-container state
// fleet-man needs to read Claude Code's run state from hooks:
//
//   1. The state-writing shell script at FleetManHookCommand,
//      copied from ClaudeHookScript bytes embedded in the binary
//      and made executable.
//
//   2. Hook entries in ~/.claude/settings.json that point at that
//      script for every event we care about, merged into the
//      user's existing settings via the safe-edit logic in
//      claude_settings.go.
//
// Idempotent: re-running Provision against a fully-provisioned
// container produces the same end state. The hook script is
// rewritten verbatim (cheap; bytes are identical) and
// InjectFleetManHooks is itself idempotent on its own output.
//
// Atomicity: settings.json is written to a same-directory tmp file
// then renamed, so a reader is never exposed to a partially-written
// document. The hook script itself is dropped in two steps (mkdir
// + cat); a crash between them just leaves a non-existent file the
// next Provision call recreates.

// ClaudeProvisioner orchestrates installation of the in-container
// state-hook script and hook entries.
type ClaudeProvisioner struct {
	exec ContainerExecutor
}

// ===========================================
// Constructors
// ===========================================

// NewClaudeProvisioner returns a provisioner that runs commands
// inside the target container via the given executor.
func NewClaudeProvisioner(exec ContainerExecutor) *ClaudeProvisioner {
	return &ClaudeProvisioner{exec: exec}
}

// ===========================================
// Public API
// ===========================================

// Provision drops the hook script and edits settings.json. Errors
// are wrapped with phase context so a partial failure tells the
// caller which step did not complete; the caller may choose to
// surface them as warnings rather than aborting container
// creation.
//
// The first step is a $HOME query — the hook script lives under
// $HOME/<FleetManScriptSuffix>, which varies per container because
// different devcontainer remoteUsers have different home dirs.
// Doing this query once at provision time and baking the absolute
// path into settings.json keeps Claude Code's hook command field
// independent of any shell expansion behaviour.
func (p *ClaudeProvisioner) Provision() error {
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
// FleetManScriptSuffix to produce the absolute path the hook script
// will be dropped at and that settings.json will reference.
func (p *ClaudeProvisioner) resolveScriptPath() (string, error) {
	out, err := p.exec.Run([]string{"sh", "-c", "echo \"$HOME\""}, nil)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", fmt.Errorf("container reported empty $HOME")
	}
	return path.Join(home, FleetManScriptSuffix), nil
}

// dropHookScript writes the embedded hook script bytes to scriptPath
// and sets the executable bit. mkdir of the parent directory
// guarantees the path exists even on a fresh container that has
// never had ~/.fleet/scripts before.
//
// All three operations are bundled into a single shell invocation
// to minimise round-trips to the container exec layer.
func (p *ClaudeProvisioner) dropHookScript(scriptPath string) error {
	parentDir := path.Dir(scriptPath)
	script := fmt.Sprintf(
		`mkdir -p %q && cat > %q && chmod +x %q`,
		parentDir, scriptPath, scriptPath,
	)
	_, err := p.exec.Run([]string{"sh", "-c", script}, ClaudeHookScript)
	return err
}

// injectSettings reads the current settings.json (or treats it as
// absent), passes it through InjectFleetManHooks, and writes the
// result back atomically.
func (p *ClaudeProvisioner) injectSettings(scriptPath string) error {
	current, err := p.readSettings()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	updated, err := InjectFleetManHooks(current, scriptPath)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	if err := p.writeSettings(updated); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readSettings cats ~/.claude/settings.json. A missing file
// produces empty bytes (treated by InjectFleetManHooks as "the file
// does not exist"), not an error — distinguishing "missing" from
// "present and empty" inside a shell pipeline is not worth the
// complexity since both yield the same downstream behaviour.
func (p *ClaudeProvisioner) readSettings() ([]byte, error) {
	const script = `if [ -f "$HOME/.claude/settings.json" ]; then cat "$HOME/.claude/settings.json"; fi`
	return p.exec.Run([]string{"sh", "-c", script}, nil)
}

// writeSettings writes content to ~/.claude/settings.json
// atomically: same-directory tmp file then rename. mkdir of
// ~/.claude handles the first-time case where Claude Code has
// never been run in this container.
func (p *ClaudeProvisioner) writeSettings(content []byte) error {
	const script = `mkdir -p "$HOME/.claude" && ` +
		`cat > "$HOME/.claude/settings.json.tmp" && ` +
		`mv "$HOME/.claude/settings.json.tmp" "$HOME/.claude/settings.json"`
	_, err := p.exec.Run([]string{"sh", "-c", script}, content)
	return err
}
