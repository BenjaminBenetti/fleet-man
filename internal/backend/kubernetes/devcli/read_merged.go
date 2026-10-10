package devcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// Config is a workspace's devcontainer configuration as the devcontainer CLI
// reads it.
type Config struct {
	// ConfigFile is the host path of the devcontainer.json in use.
	ConfigFile string
	// WorkspaceFolder is where the workspace appears in the container
	// (${containerWorkspaceFolder}).
	WorkspaceFolder string
	// Configuration is devcontainer.json as read, variables substituted.
	Configuration json.RawMessage
	// Merged is the merged configuration: the base image's metadata, each
	// feature's, then devcontainer.json's.
	Merged json.RawMessage
	// Build is the image build devcontainer.json asks for; nil for an
	// image-only configuration.
	Build *Build
	// CLIVersion is the version of the CLI that read it.
	CLIVersion string
}

// Build is a Dockerfile build: host paths of the Dockerfile and the build
// context.
type Build struct {
	Dockerfile string
	Context    string
}

// ComposeError refuses a compose devcontainer on Kubernetes. Its message
// starts with kubeshim.UnsupportedCompose.
type ComposeError struct {
	// ConfigFile is the devcontainer.json naming the compose file.
	ConfigFile string
}

func (e *ComposeError) Error() string {
	where := ""
	if e.ConfigFile != "" {
		where = " (" + e.ConfigFile + ")"
	}
	return fmt.Sprintf("%s: this is a compose devcontainer%s; compose devcontainers (dockerComposeFile) are not supported on Kubernetes", kubeshim.UnsupportedCompose, where)
}

// ReadRequest asks for a workspace's configuration.
type ReadRequest struct {
	Tools
	// WorkspaceDir is the host checkout.
	WorkspaceDir string
}

// readOutput is `devcontainer read-configuration`'s JSON.
type readOutput struct {
	Configuration       json.RawMessage `json:"configuration"`
	MergedConfiguration json.RawMessage `json:"mergedConfiguration"`
	Workspace           struct {
		WorkspaceFolder string `json:"workspaceFolder"`
	} `json:"workspace"`
}

// configFields are the parts of devcontainer.json ReadMerged looks at.
type configFields struct {
	ConfigFilePath struct {
		FsPath string `json:"fsPath"`
	} `json:"configFilePath"`
	DockerComposeFile json.RawMessage `json:"dockerComposeFile"`
	DockerFile        string          `json:"dockerFile"`
	Context           string          `json:"context"`
	Build             *struct {
		Dockerfile string `json:"dockerfile"`
		Context    string `json:"context"`
	} `json:"build"`
}

// ReadMerged reads the workspace's devcontainer configuration with
// `devcontainer read-configuration --include-merged-configuration`, through a
// capture-mode shim (so the CLI sees no existing container and reaches docker
// only for image metadata). A compose devcontainer is refused with a
// *ComposeError before anything is built: the plain configuration is read
// first, which never runs compose, and a compose call that still reaches the
// shim fails the same way.
func ReadMerged(ctx context.Context, req ReadRequest) (*Config, error) {
	docker, err := req.docker()
	if err != nil {
		return nil, err
	}
	dir, err := req.shimDir(kubeshim.Config{Mode: kubeshim.ModeCapture, Docker: docker})
	if err != nil {
		return nil, err
	}
	defer dir.Remove()
	jobLog := backend.JobLogFrom(ctx)
	base := []string{"read-configuration", "--workspace-folder", req.WorkspaceDir,
		"--docker-path", dir.Docker(), "--docker-compose-path", dir.Compose()}

	plain := req.run(ctx, base, nil, jobLog)
	if plain.err != nil {
		return nil, cliError("devcontainer read-configuration", plain)
	}
	var first readOutput
	if err := json.Unmarshal(lastLine(plain.stdout), &first); err != nil {
		return nil, fmt.Errorf("parsing devcontainer read-configuration output: %w", err)
	}
	var fields configFields
	if err := json.Unmarshal(first.Configuration, &fields); err != nil {
		return nil, fmt.Errorf("parsing devcontainer read-configuration output: %w", err)
	}
	if isSet(fields.DockerComposeFile) {
		return nil, &ComposeError{ConfigFile: fields.ConfigFilePath.FsPath}
	}

	merged := req.run(ctx, append(base, "--include-merged-configuration"), nil, jobLog)
	if composeCalled(dir) {
		return nil, &ComposeError{ConfigFile: fields.ConfigFilePath.FsPath}
	}
	if merged.err != nil {
		return nil, cliError("devcontainer read-configuration", merged)
	}
	var out readOutput
	if err := json.Unmarshal(lastLine(merged.stdout), &out); err != nil {
		return nil, fmt.Errorf("parsing devcontainer read-configuration output: %w", err)
	}
	if err := json.Unmarshal(out.Configuration, &fields); err != nil {
		return nil, fmt.Errorf("parsing devcontainer read-configuration output: %w", err)
	}
	if isSet(fields.DockerComposeFile) {
		return nil, &ComposeError{ConfigFile: fields.ConfigFilePath.FsPath}
	}
	cfg := &Config{
		ConfigFile:      fields.ConfigFilePath.FsPath,
		WorkspaceFolder: out.Workspace.WorkspaceFolder,
		Configuration:   out.Configuration,
		Merged:          out.MergedConfiguration,
		CLIVersion:      cliVersion(merged.log),
	}
	cfg.Build = buildOf(fields)
	return cfg, nil
}

// buildOf resolves a configuration's Dockerfile build the way the CLI does:
// build.dockerfile (or the legacy dockerFile) and build.context (or context,
// "." by default), both relative to the devcontainer.json.
func buildOf(f configFields) *Build {
	dockerfile, context := f.DockerFile, f.Context
	if f.Build != nil {
		if f.Build.Dockerfile != "" {
			dockerfile = f.Build.Dockerfile
		}
		if f.Build.Context != "" {
			context = f.Build.Context
		}
	}
	if dockerfile == "" || f.ConfigFilePath.FsPath == "" {
		return nil
	}
	if context == "" {
		context = "."
	}
	base := filepath.Dir(f.ConfigFilePath.FsPath)
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(base, p)
	}
	return &Build{Dockerfile: abs(dockerfile), Context: abs(context)}
}

// isSet reports whether a JSON member is present (and not null): the CLI
// takes any dockerComposeFile, even an empty one, as a compose
// configuration.
func isSet(v json.RawMessage) bool {
	return len(v) > 0 && string(v) != "null"
}

// composeCalled reports whether the CLI made a compose call through the
// shim.
func composeCalled(dir *kubeshim.Dir) bool {
	calls, err := dir.Calls()
	if err != nil {
		return false
	}
	for _, c := range calls {
		if c.Outcome == kubeshim.OutcomeCompose {
			return true
		}
	}
	return false
}

// IsCompose reports whether err is the compose refusal.
func IsCompose(err error) bool {
	var ce *ComposeError
	return errors.As(err, &ce)
}
