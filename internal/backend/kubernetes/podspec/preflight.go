package podspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// The preflight: a create asks the cluster to admit the instance before it
// builds anything, from the merged configuration alone (read-configuration
// --include-merged-configuration), since the docker run capture-up records
// comes only after the build. PreflightCapture stands in for that run with
// what decides admission: privilege, capabilities, security options, host
// namespaces, devices, mounts, sysctls, the runtime, resources (the merged
// hostRequirements). Translating and admitting it (Admit) refuses before the
// build what the real run would be refused; the real run is admitted again
// before anything is created.

// PreflightImage is the image a preflight capture names: none, since the
// image is not built yet.
const PreflightImage = "fleet.invalid/preflight"

// preflightConfig is the part of the merged configuration the
// devcontainer CLI turns into docker run options that matter at admission.
type preflightConfig struct {
	Privileged     bool              `json:"privileged"`
	Init           bool              `json:"init"`
	CapAdd         []string          `json:"capAdd"`
	SecurityOpt    []string          `json:"securityOpt"`
	RunArgs        []string          `json:"runArgs"`
	Mounts         []json.RawMessage `json:"mounts"`
	WorkspaceMount string            `json:"workspaceMount"`
	ContainerUser  string            `json:"containerUser"`
	ConfigFilePath *struct {
		FsPath string `json:"fsPath"`
	} `json:"configFilePath"`
}

// preflightMount is a mount written as an object in devcontainer.json.
type preflightMount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// preflightDropped are the runArgs options a preflight leaves out: the
// environment and the labels decide nothing at admission, and capture-up
// resolves their files and host values.
var preflightDropped = map[string]bool{"env": true, "env-file": true, "label": true, "label-file": true}

// PreflightCapture is a stand-in for the docker run capture-up will record
// for the workspace at workspaceDir (its folder in the container
// workspaceFolder), built from its merged configuration. Translate it with
// the merged configuration and no image (kubeshim.ImageConfig{}), and render
// it with the reference the image delivery will give (its registry and
// repository; the tag may stand in for the digest), since an admission
// policy may check where images come from.
func PreflightCapture(merged json.RawMessage, workspaceDir, workspaceFolder string) (*kubeshim.Capture, error) {
	var cfg preflightConfig
	if len(bytes.TrimSpace(merged)) > 0 {
		if err := json.Unmarshal(merged, &cfg); err != nil {
			return nil, fmt.Errorf("reading the merged devcontainer configuration: %w", err)
		}
	}
	labels := map[string]string{devmeta.LocalFolderLabel: workspaceDir}
	if cfg.ConfigFilePath != nil && cfg.ConfigFilePath.FsPath != "" {
		labels[devmeta.ConfigFileLabel] = cfg.ConfigFilePath.FsPath
	}
	devID := devcontainerID(labels)
	resolve := func(s string) string { return strings.ReplaceAll(s, "${devcontainerId}", devID) }

	args := []string{"--sig-proxy=false", "-a", "STDOUT", "-a", "STDERR"}
	if ws := cfg.WorkspaceMount; ws != "" {
		args = append(args, "--mount", resolve(ws))
	} else if workspaceDir != "" && workspaceFolder != "" {
		args = append(args, "--mount", "type=bind,source="+filepath.Clean(workspaceDir)+",target="+workspaceFolder)
	}
	for _, raw := range cfg.Mounts {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			args = append(args, "--mount", resolve(s))
			continue
		}
		var m preflightMount
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("reading the merged configuration's mount %s: %w", raw, err)
		}
		spec := "type=" + m.Type
		if m.Source != "" {
			spec += ",source=" + resolve(m.Source)
		}
		args = append(args, "--mount", spec+",target="+m.Target)
	}
	for _, k := range sortedKeys(labels) {
		args = append(args, "-l", k+"="+labels[k])
	}
	if cfg.Init {
		args = append(args, "--init")
	}
	if cfg.Privileged {
		args = append(args, "--privileged")
	}
	for _, c := range cfg.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, o := range cfg.SecurityOpt {
		args = append(args, "--security-opt", o)
	}
	if cfg.ContainerUser != "" {
		args = append(args, "-u", cfg.ContainerUser)
	}
	if len(cfg.RunArgs) > 0 {
		run, err := kubeshim.ParseRun(append(append([]string(nil), cfg.RunArgs...), PreflightImage))
		if err != nil {
			var unknown *kubeshim.UnknownFlagError
			if errors.As(err, &unknown) {
				return nil, &Refusal{Code: kubeshim.UnsupportedFlag(unknown.Flag), Reason: fmt.Sprintf("runArgs %s: docker run has no such option fleet knows", unknown.Flag)}
			}
			return nil, fmt.Errorf("reading the devcontainer's runArgs: %w", err)
		}
		for _, f := range run.Flags {
			if preflightDropped[f.Name] || f.Name == "group-add" && !numericID(f.Value) {
				// A group named rather than numbered resolves against the
				// built image's /etc/group (Input.LookupGroup), which the
				// admission of the real capture has and this one does not.
				continue
			}
			args = append(args, "--"+f.Name+"="+resolve(f.Value))
		}
	}
	args = append(args, PreflightImage, "/bin/sh", "-c", "exit 0")
	return &kubeshim.Capture{Format: kubeshim.CaptureFormat, Verb: "run", Args: args, Labels: labels}, nil
}

// numericID is whether s is a uid or gid docker takes as a number.
func numericID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 0
}
