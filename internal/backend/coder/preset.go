package coder

import (
	"encoding/json"
	"fmt"
)

// Preset represents a template version preset from the Coder API.
type Preset struct {
	ID          string        `json:"ID"`
	Name        string        `json:"Name"`
	Parameters  []PresetParam `json:"Parameters"`
	Description string        `json:"Description"`
}

// PresetParam is a key-value parameter inside a preset.
type PresetParam struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// FetchPresets fetches the presets for a template version via REST API.
func FetchPresets(versionID string) ([]Preset, error) {
	body, err := coderAPIGet(fmt.Sprintf("api/v2/templateversions/%s/presets", versionID))
	if err != nil {
		return nil, err
	}

	var presets []Preset
	if err := json.Unmarshal(body, &presets); err != nil {
		return nil, fmt.Errorf("parsing presets: %w", err)
	}
	return presets, nil
}
