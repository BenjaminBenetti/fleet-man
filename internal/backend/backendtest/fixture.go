package backendtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// FixtureImage is the base image of the conformance fixture: the itest
// fixtures' image, so a machine that ran the itests has it cached.
const FixtureImage = "mcr.microsoft.com/devcontainers/base:debian-12"

// fixtureDockerfile adds what the cases need that the base image lacks: tmux
// (sessions) and socat (the in-instance servers the forward case dials). It is
// built once per docker daemon and cached after that, so an instance costs a
// container, not an apt run. The base image already has git, procps, sudo for
// its vscode user, dpkg-deb, perl and coreutils.
const fixtureDockerfile = `FROM ` + FixtureImage + `
RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends tmux socat \
 && rm -rf /var/lib/apt/lists/*
`

// Paths, under the instance's $HOME, of the files the fixture's lifecycle
// commands append one line to each time they run.
const (
	PostCreateLog = ".fleet-conformance/post-create.log"
	PostStartLog  = ".fleet-conformance/post-start.log"
)

// SeedFile is a file the fixture workspace carries, and SeedContent its
// content: an instance proves its workspace is the fixture's by reading it.
const (
	SeedFile    = "conformance-seed.txt"
	SeedContent = "fleet backend conformance seed\n"
)

// FixtureRepo is the name of the fixture workspace folder, which devcontainer
// mounts at /workspaces/<name>.
const FixtureRepo = "fixture"

// Fixture is what an instance is created from: the conformance fixture, with
// optional changes for a scenario.
type Fixture struct {
	// PostCreate, when set, replaces the fixture's postCreateCommand (a
	// failing one makes a failed create).
	PostCreate string
	// Privileged runs the instance with --privileged (what docker-in-docker
	// asks for): on a cluster, the pod whose user processes a consistent
	// clone can freeze.
	Privileged bool
	// BlockingLoginShell makes the remote user's interactive shells block
	// (BlockingShellWait) until UnblockLoginShell exists: the devcontainer
	// CLI's userEnvProbe (`bash -lic`) never answers, as with an rc file that
	// waits for input.
	BlockingLoginShell bool
}

// BlockingShellWait is how long a BlockingLoginShell fixture's interactive
// shell blocks, and UnblockLoginShell the file that ends it at once.
const (
	BlockingShellWait = 15 * time.Minute
	UnblockLoginShell = "/tmp/fleet-conformance-unblock"
)

// blockingBashrc is the line a BlockingLoginShell fixture appends to the
// remote user's ~/.bashrc, which `bash -lic` reads (through ~/.profile).
var blockingBashrc = fmt.Sprintf(`case $- in *i*) [ -e %s ] || sleep %d;; esac`, UnblockLoginShell, int(BlockingShellWait/time.Second))

func (f Fixture) postCreate() string {
	if f.PostCreate != "" {
		return f.PostCreate
	}
	return countInto(PostCreateLog, "created")
}

// countInto is a lifecycle command appending word to the $HOME-relative log.
func countInto(log, word string) string {
	return fmt.Sprintf(`mkdir -p "$HOME/%s" && echo %s >> "$HOME/%s"`, filepath.Dir(log), word, log)
}

// devcontainerJSON is the subset of devcontainer.json the fixture sets.
type devcontainerJSON struct {
	Name              string            `json:"name"`
	Build             map[string]string `json:"build"`
	RemoteUser        string            `json:"remoteUser"`
	PostCreateCommand string            `json:"postCreateCommand"`
	PostStartCommand  string            `json:"postStartCommand"`
	RunArgs           []string          `json:"runArgs,omitempty"`
}

// Write lays the fixture out in dir, which must not exist yet: a
// .devcontainer folder (Dockerfile and devcontainer.json) and SeedFile.
func (f Fixture) Write(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		return err
	}
	dc := devcontainerJSON{
		Name:              "fleet backend conformance",
		Build:             map[string]string{"dockerfile": "Dockerfile"},
		RemoteUser:        "vscode",
		PostCreateCommand: f.postCreate(),
		PostStartCommand:  countInto(PostStartLog, "started"),
	}
	if f.Privileged {
		dc.RunArgs = []string{"--privileged"}
	}
	config, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return err
	}
	dockerfile := fixtureDockerfile
	if f.BlockingLoginShell {
		dockerfile += fmt.Sprintf("RUN echo %s >> /home/vscode/.bashrc\n", shellquote.Single(blockingBashrc))
	}
	files := map[string]string{
		".devcontainer/devcontainer.json": string(config) + "\n",
		".devcontainer/Dockerfile":        dockerfile,
		SeedFile:                          SeedContent,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
