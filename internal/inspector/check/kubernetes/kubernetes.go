// Package kubernetes is an inspector check: what a repository's
// devcontainer.json asks of a Kubernetes cluster, read before anything is
// built, and what that means on one cluster. The New instance dialog's
// Deploy row shows it when a cluster is picked (ListDeployTargets), so a
// compose repo, Docker-in-Docker on a cluster that allows no privileged pods
// or a prebuilt image on a cluster without root says so before the create.
//
// It is a reading of devcontainer.json alone, not of the merged
// configuration a create reads (image metadata, each feature's own): a
// create's preflight decides for itself, from that configuration and the
// cluster's admission, and refuses with the exact reason
// (kubernetes:unsupported:*). The findings here are the ones the file
// decides, named the way the create would refuse them.
package kubernetes

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/devcontainer"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

// ComposeRefusal is why no cluster takes a compose devcontainer.
const ComposeRefusal = "compose devcontainers are not supported on Kubernetes"

// The names a finding gives what asks for an engine or for privilege.
const (
	dindFeature    = "Docker-in-Docker"
	doodFeature    = "Docker outside of Docker"
	privilegedFlag = "--privileged"
	dockerSockBind = "a docker.sock bind"
	apiSocketFlag  = "--use-api-socket"
)

// Config is what a devcontainer.json asks of a cluster.
type Config struct {
	// Compose: a compose devcontainer (dockerComposeFile), which no
	// cluster runs.
	Compose bool
	// Privileged names what asks for a privileged dev container
	// (Docker-in-Docker, the docker-in-docker feature; --privileged, the
	// privileged property or run argument); "" when nothing does.
	Privileged string
	// DooD names what binds the docker engine's socket (Docker outside of
	// Docker, the feature; a docker.sock bind in mounts or runArgs;
	// --use-api-socket), which on a cluster gets the pod's own engine, a
	// sidecar that runs as root and privileged; "" when nothing does.
	DooD string
	// Prebuilt is the image a configuration runs as it is: one named by
	// image, with no Dockerfile and no features, which the devcontainer CLI
	// never builds, so fleet cannot add anything to it (the audio stack a
	// cluster without root needs). "" otherwise.
	Prebuilt string
}

// Read reads repo's devcontainer.json (inspector.ErrNoDevcontainerConfig
// without one).
func Read(repo *inspector.Repo) (Config, error) {
	_, data, err := repo.FindDevcontainerJSON()
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// document is the part of devcontainer.json the check reads.
type document struct {
	Image             string            `json:"image"`
	DockerFile        string            `json:"dockerFile"`
	DockerFileLower   string            `json:"dockerfile"`
	Build             *buildSection     `json:"build"`
	DockerComposeFile json.RawMessage   `json:"dockerComposeFile"`
	Features          map[string]any    `json:"features"`
	Privileged        bool              `json:"privileged"`
	RunArgs           []string          `json:"runArgs"`
	Mounts            []json.RawMessage `json:"mounts"`
}

type buildSection struct {
	Dockerfile string `json:"dockerfile"`
}

// Parse reads a devcontainer.json (JSONC: comments and trailing commas are
// allowed).
func Parse(data []byte) (Config, error) {
	var doc document
	if err := json.Unmarshal(devcontainer.ToStrictJSON(data), &doc); err != nil {
		return Config{}, fmt.Errorf("devcontainer.json: %w", err)
	}
	var cfg Config
	cfg.Compose = composeFile(doc.DockerComposeFile)
	for id := range doc.Features {
		switch featureName(id) {
		case "docker-in-docker":
			cfg.Privileged = dindFeature
		case "docker-outside-of-docker", "docker-from-docker":
			cfg.DooD = doodFeature
		}
	}
	if cfg.Privileged == "" && (doc.Privileged || hasFlag(doc.RunArgs, privilegedFlag)) {
		cfg.Privileged = privilegedFlag
	}
	if cfg.DooD == "" {
		switch {
		case runArgsBindDockerSocket(doc.RunArgs) || mountsBindDockerSocket(doc.Mounts):
			cfg.DooD = dockerSockBind
		case hasFlag(doc.RunArgs, apiSocketFlag):
			cfg.DooD = apiSocketFlag
		}
	}
	built := doc.DockerFile != "" || doc.DockerFileLower != "" || (doc.Build != nil && doc.Build.Dockerfile != "")
	if doc.Image != "" && !built && !cfg.Compose && len(doc.Features) == 0 {
		cfg.Prebuilt = doc.Image
	}
	return cfg, nil
}

// composeFile reports whether dockerComposeFile names a file (a string, or
// a list of them).
func composeFile(raw json.RawMessage) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return strings.TrimSpace(one) != ""
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, f := range many {
			if strings.TrimSpace(f) != "" {
				return true
			}
		}
	}
	return false
}

// featureName is a feature id's name: its last path element without a tag
// or digest ("ghcr.io/devcontainers/features/docker-in-docker:2" is
// docker-in-docker, as is a local "./docker-in-docker").
func featureName(id string) string {
	name := path.Base(strings.TrimSuffix(strings.TrimSpace(id), "/"))
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	return strings.ToLower(name)
}

// hasFlag reports whether args carry flag, alone or as flag=true.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || a == flag+"=true" {
			return true
		}
	}
	return false
}

// isDockerSocket reports whether a bind's source is the docker engine's
// socket, which is known by its name (/var/run/docker.sock, a rootless
// engine's or Docker Desktop's), as the create's translation knows it.
func isDockerSocket(source string) bool {
	return source != "" && path.Base(source) == "docker.sock"
}

// runArgsBindDockerSocket reports whether a -v / --volume / --mount run
// argument binds the docker engine's socket.
func runArgsBindDockerSocket(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		var value string
		switch {
		case a == "-v" || a == "--volume" || a == "--mount":
			if i+1 < len(args) {
				i++
				value = args[i]
			}
		case strings.HasPrefix(a, "--volume="), strings.HasPrefix(a, "--mount="), strings.HasPrefix(a, "-v="):
			value = a[strings.Index(a, "=")+1:]
		case strings.HasPrefix(a, "-v") && len(a) > 2:
			value = a[2:]
		default:
			continue
		}
		if strings.HasPrefix(a, "--mount") {
			if isDockerSocket(mountOption(value, "source", "src")) {
				return true
			}
			continue
		}
		if source, _, ok := strings.Cut(value, ":"); ok && isDockerSocket(source) {
			return true
		}
	}
	return false
}

// mountsBindDockerSocket reports whether a mounts entry (the
// "source=…,target=…" string form, or the object form) binds the docker
// engine's socket.
func mountsBindDockerSocket(mounts []json.RawMessage) bool {
	for _, raw := range mounts {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if isDockerSocket(mountOption(s, "source", "src")) {
				return true
			}
			continue
		}
		var obj struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(raw, &obj) == nil && isDockerSocket(obj.Source) {
			return true
		}
	}
	return false
}

// mountOption is the value of the first of keys in a --mount style
// "k=v,k=v" list.
func mountOption(spec string, keys ...string) string {
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		for _, key := range keys {
			if k == key {
				return v
			}
		}
	}
	return ""
}

// Cluster is what the findings need to know of one cluster connection, from
// its last checks.
type Cluster struct {
	// Name is the connection's display name.
	Name string
	// Known: the cluster's checks have run, so the fields below say what it
	// allows. A cluster not checked yet gets only the findings that hold on
	// every cluster.
	Known bool
	// NoRoot: the namespace admits no root pods, so instances run as one
	// non-root user (no privilege, no root engine, nothing installed).
	NoRoot bool
	// PrivilegedAllowed is the connection's effective Allow privileged pods
	// (auto resolved: on for a local cluster).
	PrivilegedAllowed bool
	// Sysbox: a sysbox runtime class serves privileged dev containers and
	// the docker engine's sidecar without privilege on the node.
	Sysbox bool
	// Version is the API server's version ("v1.31.2").
	Version string
}

// Findings is what a create on one cluster would meet.
type Findings struct {
	// Refusal says why no create of this repository can go to the cluster:
	// its Deploy target is unavailable with this reason. "" when one can.
	Refusal string
	// Warnings are what a create there would be refused for, or lack, as
	// the configuration stands, each naming the cluster: shown when it is
	// picked.
	Warnings []string
}

// Check is what cfg means on cluster c; audio says whether the virtual
// microphone or audio output is on (the daemon's settings), which only then
// concern a cluster without root.
func Check(cfg Config, c Cluster, audio bool) Findings {
	if cfg.Compose {
		return Findings{Refusal: ComposeRefusal}
	}
	if !c.Known {
		return Findings{}
	}
	var out Findings
	add := func(format string, args ...any) {
		out.Warnings = append(out.Warnings, c.Name+": "+fmt.Sprintf(format, args...))
	}
	if what := cfg.Privileged; what != "" {
		switch {
		case c.NoRoot:
			add("%s needs root, and instances there run without it", what)
		case !c.PrivilegedAllowed && !c.Sysbox:
			add("%s needs privileged pods, which are not allowed there (Allow privileged pods, Settings → Kubernetes)", what)
		}
	}
	if what := cfg.DooD; what != "" {
		switch {
		case c.NoRoot:
			add("%s gets a docker engine sidecar there, which needs root; instances there run without it", what)
		case !c.PrivilegedAllowed && !c.Sysbox:
			add("%s gets a docker engine sidecar there, which needs privileged pods (Allow privileged pods, Settings → Kubernetes)", what)
		case minorBelow(c.Version, 29):
			add("%s gets a docker engine sidecar there, which needs Kubernetes 1.29 or later; it runs %s", what, c.Version)
		}
	}
	if audio {
		if w := AudioWarning(cfg, c); w != "" {
			out.Warnings = append(out.Warnings, w)
		}
	}
	return out
}

// AudioWarning is the finding of a prebuilt image on a cluster without root,
// "" for any other: fleet gives an image it builds there the audio stack in
// a layer of its own, and a prebuilt image is never built, so it has audio
// only when it carries the stack itself (which the create checks once the
// pod runs).
func AudioWarning(cfg Config, c Cluster) string {
	if cfg.Compose || cfg.Prebuilt == "" || !c.Known || !c.NoRoot {
		return ""
	}
	return c.Name + ": no audio unless " + cfg.Prebuilt + " has PulseAudio and ALSA: instances there run without root, and fleet adds the audio stack only to images it builds"
}

// minorBelow reports whether a Kubernetes 1.x version reads as below 1.min
// ("v1.28.3-gke.100" is below 29); one it cannot read is not.
func minorBelow(version string, min int) bool {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "1.")
	if !ok {
		return false
	}
	n := 0
	digits := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		digits++
	}
	return digits > 0 && n < min
}
