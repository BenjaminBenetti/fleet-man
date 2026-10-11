package podspec

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// mergedConfig is the part of the merged devcontainer configuration
// Translate reads: what `devcontainer exec` knows about the instance
// (exec.json) and the host requirements.
type mergedConfig struct {
	RemoteUser   string `json:"remoteUser"`
	UserEnvProbe string `json:"userEnvProbe"`
	// RemoteEnv values are nil for a variable set to null.
	RemoteEnv        map[string]*string `json:"remoteEnv"`
	HostRequirements *hostRequirements  `json:"hostRequirements"`
}

// hostRequirements is the merged hostRequirements: the largest of each
// value across the image, the features and devcontainer.json. The CLI
// writes memory and storage as byte counts; devcontainer.json itself spells
// them with tb/gb/mb/kb.
type hostRequirements struct {
	CPUs    float64         `json:"cpus"`
	Memory  json.RawMessage `json:"memory"`
	Storage json.RawMessage `json:"storage"`
	// GPU is true, false, "optional" or an object of minimums.
	GPU json.RawMessage `json:"gpu"`
}

// parseMerged reads raw (the merged configuration); empty reads as an
// empty configuration.
func parseMerged(raw json.RawMessage) (*mergedConfig, error) {
	var m mergedConfig
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("reading the merged devcontainer configuration: %w", err)
	}
	return &m, nil
}

// size reads a hostRequirements memory or storage value in bytes; 0 when it
// is absent. The CLI merges the largest value across the image's metadata
// layers, which for an image with none (busybox) is JavaScript's -Infinity,
// written as "-Infinity": no requirement either.
func (h *hostRequirements) size(raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte(`"-Infinity"`)) {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return 0, fmt.Errorf("invalid size %s", raw)
		}
		s = n.String()
	}
	return devcontainerBytes(s)
}

// wantsGPU reports whether hostRequirements asks for a GPU it cannot do
// without (true, or an object of minimums; "optional" can).
func (h *hostRequirements) wantsGPU() bool {
	raw := bytes.TrimSpace(h.GPU)
	if len(raw) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	return raw[0] == '{'
}
