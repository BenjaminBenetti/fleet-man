package kubeshim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Mode is what the shim is for in one directory.
type Mode string

const (
	// ModeCapture serves `devcontainer up` and `read-configuration` on the
	// daemon host: image builds and pulls pass through to the real docker,
	// the container verbs see an empty engine, and the docker run is captured
	// instead of run.
	ModeCapture Mode = "capture"
	// ModePod serves `devcontainer set-up` for an instance's pod: inspect
	// answers with the pod's Container and exec runs in the pod through the
	// exec socket.
	ModePod Mode = "pod"
)

// Config is a shim directory's kubeshim.json.
type Config struct {
	Mode Mode `json:"mode"`
	// Docker is the real docker binary (absolute) capture mode hands builds,
	// pulls and image inspects to.
	Docker string `json:"docker,omitempty"`
	// Pod is pod mode's container and exec socket.
	Pod *PodConfig `json:"pod,omitempty"`
}

// PodConfig is what pod mode answers from.
type PodConfig struct {
	// Container is what inspect reports and what exec may name.
	Container Container `json:"container"`
	// ExecSocket is the unix socket an exec is sent down (ServeExec serves
	// it).
	ExecSocket string `json:"execSocket"`
}

// Files in a shim directory.
const (
	// ConfigFile is the mode file the shim reads beside its own link.
	ConfigFile = "kubeshim.json"
	// captureFile is where capture mode writes the Capture (secrets
	// included, mode 0600).
	captureFile = "capture.raw.json"
	// callsFile is the shim's journal, one JSON Call per line.
	callsFile = "calls.jsonl"
	// execSocketFile is the default name of pod mode's exec socket.
	execSocketFile = "exec.sock"
	// dockerLink and composeLink are the names the devcontainer CLI is given
	// as --docker-path and --docker-compose-path.
	dockerLink  = "docker"
	composeLink = "docker-compose"
)

// validate checks c can drive a shim.
func (c *Config) validate() error {
	switch c.Mode {
	case ModeCapture:
		if c.Docker != "" && !filepath.IsAbs(c.Docker) {
			return fmt.Errorf("kubeshim: docker path %q is not absolute", c.Docker)
		}
	case ModePod:
		if c.Pod == nil || c.Pod.ExecSocket == "" || c.Pod.Container.ContainerID == "" {
			return errors.New("kubeshim: pod mode needs a container and an exec socket")
		}
	default:
		return fmt.Errorf("kubeshim: unknown mode %q", c.Mode)
	}
	return nil
}

// loadConfig reads the kubeshim.json in dir.
func loadConfig(dir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, ConfigFile), err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
