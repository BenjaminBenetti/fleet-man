package agentstrategy

import (
	"embed"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
)

// The fleet status mod is a Claude Code mod — a plugin of function hooks — that
// draws a band above the prompt: the fleet/instance this Claude Code runs in,
// the agents working and idle across every fleet, the ones that just stopped,
// and whether the fleet MCP (Fleet Admiral) is connected. Its source lives in
// statusmod/ (TypeScript, tested with `claude plugin test`) and ships inside
// the fleet binary. Every devcontainer instance's shells install it (fleet.rc
// runs `fleet claude-mod-env`, which calls InstallStatusMod) and load it
// through CLAUDE_CODE_PLUGIN_DIRS, like the fleet MCP plugin. The daemon feeds
// it through StatusModFile, kept in the instance's control directory while the
// global "Fleet status mod" setting is on; without that file it draws nothing,
// so the setting takes effect in running sessions too.

const (
	// StatusModName is the mod's plugin name.
	StatusModName = "fleet-status"
	// StatusModFileName is the basename of the status file the daemon writes
	// into each instance's control directory for the mod. The mod reads it at
	// /fleet-mounts/control/<StatusModFileName> (hooks/view.ts STATUS_FILE).
	StatusModFileName = "claude-status.json"
	// statusModManifest is the manifest's path inside the mod.
	statusModManifest = ".claude-plugin/plugin.json"
)

// statusModSource is the mod as it ships: the manifest, the hooks module and
// its state contract. The tests and the tsconfig stay behind.
//
//go:embed statusmod/.claude-plugin/plugin.json statusmod/hooks statusmod/types
var statusModSource embed.FS

// StatusModFile is the status file's content (hooks/view.ts Status).
type StatusModFile struct {
	// UpdatedAt is when the daemon wrote the file, in Unix milliseconds; it
	// is rewritten every few seconds while the counts are live.
	UpdatedAt int64 `json:"updated_at"`
	// Fleet and Instance name the instance the file is for (the instance's
	// display name).
	Fleet    string `json:"fleet"`
	Instance string `json:"instance"`
	// Working and Idle count the agents of every fleet's running instances.
	Working int `json:"working"`
	Idle    int `json:"idle"`
	// Stops are the agents of other instances that went from working to idle
	// in the last moments, oldest first.
	Stops []StatusModStop `json:"stops"`
}

// StatusModStop is one agent that stopped working.
type StatusModStop struct {
	Fleet    string `json:"fleet"`
	Instance string `json:"instance"`
	// At is when it stopped, in Unix milliseconds.
	At int64 `json:"at"`
}

// statusModSetup returns the mod's files under dir and the environment that
// loads it. The manifest's version is fleet's own.
func statusModSetup(dir string) (Setup, error) {
	root := path.Join(dir, StatusModName)
	var files []File
	err := fs.WalkDir(statusModSource, "statusmod", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := statusModSource.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "statusmod/")
		if rel == statusModManifest {
			if content, err = withVersion(content, pluginVersion()); err != nil {
				return err
			}
		}
		files = append(files, File{Path: path.Join(root, rel), Content: content, Mode: 0o644})
		return nil
	})
	if err != nil {
		return Setup{}, err
	}
	return Setup{
		Files: files,
		Env:   []EnvVar{{Name: claudePluginDirsEnv, Value: root, PathList: true}},
	}, nil
}

// withVersion sets a plugin manifest's version.
func withVersion(manifest []byte, version string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, err
	}
	m["version"] = version
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// InstallStatusMod writes the fleet status mod under dir and returns the shell
// code that loads it into the Claude Code sessions started from the shell
// (Setup.Exports). Like InstallFleetMCP it runs inside the instance at the
// start of every shell, so a file is only rewritten when its content changed:
// a running session watches the folder and reloads the mod on a change.
func InstallStatusMod(dir string) (string, error) {
	setup, err := statusModSetup(dir)
	if err != nil {
		return "", err
	}
	for _, f := range setup.Files {
		if err := writeIfChanged(f); err != nil {
			return "", err
		}
	}
	return setup.Exports(), nil
}
