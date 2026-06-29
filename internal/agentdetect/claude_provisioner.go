package agentdetect

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
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
	home, err := p.resolveHome()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	scriptPath := path.Join(home, FleetManScriptSuffix)
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

// settingsSuffix is the home-relative path of Claude Code's settings file.
const settingsSuffix = ".claude/settings.json"

// resolveHome asks the container for $HOME. The hook script and settings.json
// both live under it, and their absolute paths are baked into the in-container
// commands (and into settings.json's hook command field) so nothing depends on
// later shell expansion.
func (p *ClaudeProvisioner) resolveHome() (string, error) {
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
// the executable bit. It uses an inline (base64-in-argv) write rather than
// streaming the bytes over stdin: a stdin-reading `cat > file` hangs forever on
// the coder backend, whose transport never delivers the stdin EOF (issue #223).
// The write mkdir's the parent and is atomic.
func (p *ClaudeProvisioner) dropHookScript(scriptPath string) error {
	argv, err := backend.InlineWriteScript(scriptPath, ClaudeHookScript, 0o755)
	if err != nil {
		return err
	}
	_, err = p.exec.Run(argv, nil)
	return err
}

// injectSettings reads the current settings.json (or treats it as
// absent), passes it through InjectFleetManHooks, and writes the
// result back atomically.
func (p *ClaudeProvisioner) injectSettings(home, scriptPath string) error {
	settingsPath := path.Join(home, settingsSuffix)
	current, err := p.readSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	updated, err := InjectFleetManHooks(current, scriptPath)
	if err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	if err := p.writeSettings(settingsPath, updated); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readSettings cats settingsPath. A missing file produces empty bytes (treated
// by InjectFleetManHooks as "the file does not exist"), not an error —
// distinguishing "missing" from "present and empty" inside a shell pipeline is
// not worth the complexity since both yield the same downstream behaviour. The
// read produces output and exits, so it has no stdin-EOF dependency.
func (p *ClaudeProvisioner) readSettings(settingsPath string) ([]byte, error) {
	q := backend.ShellQuote(settingsPath)
	script := fmt.Sprintf(`if [ -f %s ]; then cat %s; fi`, q, q)
	return p.exec.Run([]string{"sh", "-c", script}, nil)
}

// writeSettings writes content to settingsPath atomically (same-directory tmp
// then rename, plus a parent mkdir for the first-time case — both guaranteed by
// the CopyFile transport). It streams via CopyFile rather than an inline
// (base64-in-argv) write because the merged settings.json is the one unbounded
// payload here: a user's pre-existing file can be arbitrarily large, and the
// inline writer hard-errors above its ARG_MAX-derived cap. CopyFile is both
// uncapped and stdin-EOF-safe on every backend including coder (issue #223), so
// it carries a large settings.json without the hang the inline path was added to
// avoid. The fixed-size hook script keeps using the inline writer.
func (p *ClaudeProvisioner) writeSettings(settingsPath string, content []byte) error {
	return p.exec.CopyFile(bytes.NewReader(content), settingsPath, 0o644)
}
