package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// DefaultDataFolder is where set-up keeps its marker files in the pod (on the
// persisted root, so they survive stop and start, and a rebuild's fresh root
// runs every hook again).
const DefaultDataFolder = "/var/lib/fleet/devcontainer"

// SetUpRequest asks for an instance's lifecycle hooks to be run in its pod.
type SetUpRequest struct {
	Tools
	// Container is the pod's container as inspect reports it: ContainerFor
	// of the captured run, with the pod's ContainerID, Name, Created (the
	// container-created annotation) and StartedAt (kubeshim.StartedAt).
	Container kubeshim.Container
	// WorkspaceFolder is the workspace's folder in the container
	// (${containerWorkspaceFolder}).
	WorkspaceFolder string
	// Execer runs the CLI's execs in the pod (the agent's explicit class).
	Execer kubeshim.Execer
	// DataFolder is set-up's marker folder in the pod; "" means
	// DefaultDataFolder. A pod without root (every class runs as the pod's
	// uid) needs one that uid can create.
	DataFolder string
	// SkipPostCreate runs no hooks (--skip-post-create).
	SkipPostCreate bool
}

// SetUpResult is a set-up's outcome.
type SetUpResult struct {
	// Merged is the merged configuration set-up ran with.
	Merged json.RawMessage
	// CLIVersion is the devcontainer CLI's version.
	CLIVersion string
	// Calls is every docker call the CLI made, environment values redacted.
	Calls []kubeshim.Call
}

// SetUp runs the devcontainer lifecycle in an instance's pod with the
// official `devcontainer set-up --container-id <id> --docker-path <shim>`,
// through a pod-mode shim: inspect answers with req.Container, exec runs in
// the pod through req.Execer. set-up keeps the reference semantics: it runs
// onCreate, updateContent and postCreate when the container is new (Created
// differs from their markers), postStart once per start (StartedAt), and
// postAttach every time, with remoteEnv and userEnvProbe applied.
//
// The data folder is made first, owned by the remote user: set-up writes the
// markers as that user and silently skips every marker-gated hook when it
// cannot. A failure is returned for the caller to treat as fatal (create,
// rebuild) or as a warning (start). Its log goes to ctx's provisioning log.
func SetUp(ctx context.Context, req SetUpRequest) (*SetUpResult, error) {
	if req.Container.ContainerID == "" || req.Execer == nil {
		return nil, errors.New("devcli.SetUp needs a container and an Execer")
	}
	// The markers compare these: a zero Created would run the creation hooks
	// once ever, a zero StartedAt postStart once ever.
	if req.Container.Created.IsZero() || req.Container.StartedAt.IsZero() {
		return nil, errors.New("devcli.SetUp needs the container's Created and StartedAt")
	}
	folder := req.DataFolder
	if folder == "" {
		folder = DefaultDataFolder
	}
	if err := prepareDataFolder(ctx, req.Execer, folder, req.Container.RemoteUser()); err != nil {
		return nil, err
	}

	dir, err := req.shimDir(kubeshim.Config{Mode: kubeshim.ModePod, Pod: &kubeshim.PodConfig{Container: req.Container}})
	if err != nil {
		return nil, err
	}
	defer dir.Remove()
	ln, err := net.Listen("unix", dir.ExecSocket())
	if err != nil {
		return nil, fmt.Errorf("listening for the docker shim's execs: %w", err)
	}
	serveCtx, stopServing := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- kubeshim.ServeExec(serveCtx, ln, req.Execer) }()
	defer func() {
		stopServing()
		<-served
	}()

	// set-up takes the workspace folder from the configuration it is given.
	// The real devcontainer.json is left out: its hooks are already in the
	// metadata label, and giving it would run them twice.
	configFile := filepath.Join(dir.Path(), "devcontainer.json")
	synthetic := map[string]string{}
	if req.WorkspaceFolder != "" {
		synthetic["workspaceFolder"] = req.WorkspaceFolder
	}
	config, _ := json.Marshal(synthetic)
	if err := os.WriteFile(configFile, config, 0o600); err != nil {
		return nil, err
	}

	args := []string{"set-up", "--container-id", req.Container.ContainerID,
		"--docker-path", dir.Docker(), "--config", configFile,
		"--container-data-folder", folder, "--include-merged-configuration"}
	if req.SkipPostCreate {
		args = append(args, "--skip-post-create")
	}
	r := req.run(ctx, args, nil, backend.JobLogFrom(ctx))
	res := &SetUpResult{CLIVersion: cliVersion(r.log)}
	res.Calls, _ = dir.Calls()
	if r.err != nil {
		return res, withRefusals(cliError("devcontainer set-up (lifecycle hooks)", r), res.Calls)
	}
	var out struct {
		Outcome             string          `json:"outcome"`
		MergedConfiguration json.RawMessage `json:"mergedConfiguration"`
	}
	if err := json.Unmarshal(lastLine(r.stdout), &out); err != nil || out.Outcome != "success" {
		return res, cliError("devcontainer set-up (lifecycle hooks)", r)
	}
	res.Merged = out.MergedConfiguration
	flog.Info("kubernetes set-up", "container", req.Container.ContainerID, "cli", res.CLIVersion)
	return res, nil
}

// prepareDataFolder makes set-up's marker folder and gives it to the remote
// user, then checks the user can write it.
func prepareDataFolder(ctx context.Context, ex kubeshim.Execer, folder, user string) error {
	owner, _, _ := strings.Cut(user, ":")
	const mkdir = `mkdir -p -- "$1" && if [ "$(id -u)" = 0 ]; then chown -- "$2" "$1"; fi`
	if err := execOK(ctx, ex, kubeshim.ExecRequest{User: "root", Argv: []string{"sh", "-c", mkdir, "sh", folder, owner}}); err != nil {
		return fmt.Errorf("making the devcontainer data folder %s for %s: %w", folder, owner, err)
	}
	if err := execOK(ctx, ex, kubeshim.ExecRequest{User: user, Argv: []string{"sh", "-c", `test -w "$1"`, "sh", folder}}); err != nil {
		return fmt.Errorf("the devcontainer data folder %s is not writable by %s, so set-up would skip the lifecycle hooks: %w", folder, user, err)
	}
	return nil
}

// execOK runs req and fails unless it exits 0.
func execOK(ctx context.Context, ex kubeshim.Execer, req kubeshim.ExecRequest) error {
	var out bytes.Buffer
	code, err := ex.Exec(ctx, req, kubeshim.ExecStdio{Stdout: &out, Stderr: &out})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(out.String()))
	}
	return nil
}
