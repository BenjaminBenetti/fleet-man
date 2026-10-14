package devmeta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// editor.go: what an editor attaching to an instance applies from its
// merged devcontainer metadata, the way VS Code's Dev Containers extension
// applies it on attach: the customizations.vscode extensions and settings,
// the port attributes, and postAttachCommand. Over Remote-SSH (Kubernetes
// instances, and devcontainer instances of a remote daemon) the
// in-instance `fleet ssh-server` applies it instead, from the EditorConfig
// the daemon keeps in the instance's control directory.

// EditorConfigName is the control file the daemon writes the instance's
// EditorConfig to, and EditorConfigPath where `fleet ssh-server` reads it
// inside the instance (its control directory, /fleet-mounts/control).
const (
	EditorConfigName = "editor.json"
	EditorConfigPath = "/fleet-mounts/control/" + EditorConfigName
)

// EditorConfigVersion is the EditorConfig format this fleet writes.
const EditorConfigVersion = 1

// The machine settings the port attributes become.
const (
	PortsAttributesSetting      = "remote.portsAttributes"
	OtherPortsAttributesSetting = "remote.otherPortsAttributes"
)

// EditorConfig is what `fleet ssh-server` applies for VS Code.
type EditorConfig struct {
	V int `json:"v"`
	// Gen names the instance's container generation (its ContainerID): an
	// extension is attempted once per Gen.
	Gen string `json:"gen"`
	// WorkspaceFolder is the folder VS Code opens, which postAttachCommand
	// runs in.
	WorkspaceFolder string `json:"workspaceFolder,omitempty"`
	// Extensions are the extension ids to install, in order.
	Extensions []string `json:"extensions,omitempty"`
	// Settings are the machine settings to merge: customizations.vscode
	// settings, plus remote.portsAttributes and remote.otherPortsAttributes
	// from portsAttributes and otherPortsAttributes.
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
	// PostAttach is each metadata entry's postAttachCommand, in order: a
	// string (run by /bin/sh -c), an array (run as is) or an object of
	// named commands (run in parallel).
	PostAttach []json.RawMessage `json:"postAttach,omitempty"`
}

// metadataEntry is the part of one devcontainer.metadata entry an editor
// applies.
type metadataEntry struct {
	Customizations struct {
		VSCode struct {
			Extensions []string                   `json:"extensions"`
			Settings   map[string]json.RawMessage `json:"settings"`
		} `json:"vscode"`
	} `json:"customizations"`
	// The pre-customizations spelling, which older configs still use.
	Extensions           []string                   `json:"extensions"`
	Settings             map[string]json.RawMessage `json:"settings"`
	PostAttachCommand    json.RawMessage            `json:"postAttachCommand"`
	PortsAttributes      map[string]json.RawMessage `json:"portsAttributes"`
	OtherPortsAttributes json.RawMessage            `json:"otherPortsAttributes"`
}

// EditorFromMetadata merges a devcontainer.metadata label value (the JSON
// array of entries: image, features, then devcontainer.json) as the Dev
// Containers extension does: extensions in order of first mention, an id
// written "-<id>" dropping one an earlier entry listed (ids compare without
// case); settings key by key, later entries winning; portsAttributes port
// by port, later entries winning; the last otherPortsAttributes; every
// postAttachCommand in order. An empty label is an empty config.
func EditorFromMetadata(metadataJSON string) (EditorConfig, error) {
	cfg := EditorConfig{V: EditorConfigVersion}
	if strings.TrimSpace(metadataJSON) == "" {
		return cfg, nil
	}
	var entries []metadataEntry
	if err := json.Unmarshal([]byte(metadataJSON), &entries); err != nil {
		return cfg, fmt.Errorf("devcontainer metadata: %w", err)
	}
	var (
		exts      []string
		settings  = map[string]json.RawMessage{}
		ports     = map[string]json.RawMessage{}
		otherPort json.RawMessage
	)
	for _, e := range entries {
		for _, id := range append(e.Extensions, e.Customizations.VSCode.Extensions...) {
			exts = mergeExtension(exts, strings.TrimSpace(id))
		}
		for k, v := range e.Settings {
			settings[k] = v
		}
		for k, v := range e.Customizations.VSCode.Settings {
			settings[k] = v
		}
		for port, attrs := range e.PortsAttributes {
			ports[port] = attrs
		}
		if present(e.OtherPortsAttributes) {
			otherPort = e.OtherPortsAttributes
		}
		if present(e.PostAttachCommand) {
			cfg.PostAttach = append(cfg.PostAttach, e.PostAttachCommand)
		}
	}
	if len(ports) > 0 {
		merged := map[string]json.RawMessage{}
		if raw, ok := settings[PortsAttributesSetting]; ok {
			_ = json.Unmarshal(raw, &merged) // a non-object setting is replaced
			if merged == nil {
				merged = map[string]json.RawMessage{}
			}
		}
		for port, attrs := range ports {
			merged[port] = attrs
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return cfg, err
		}
		settings[PortsAttributesSetting] = data
	}
	if otherPort != nil {
		settings[OtherPortsAttributesSetting] = otherPort
	}
	cfg.Extensions = exts
	if len(settings) > 0 {
		cfg.Settings = settings
	}
	return cfg, nil
}

// mergeExtension adds id to exts, or for "-<id>" removes it.
func mergeExtension(exts []string, id string) []string {
	if id == "" || id == "-" {
		return exts
	}
	drop := strings.HasPrefix(id, "-")
	key := strings.ToLower(strings.TrimPrefix(id, "-"))
	for i, have := range exts {
		if strings.ToLower(have) == key {
			if drop {
				return append(exts[:i:i], exts[i+1:]...)
			}
			return exts
		}
	}
	if drop {
		return exts
	}
	return append(exts, id)
}

// present reports whether a raw JSON value is there and not null.
func present(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}
