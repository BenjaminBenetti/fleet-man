package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// CaptureRequest asks for an instance's image to be built on the daemon host
// and its docker run captured.
type CaptureRequest struct {
	Tools
	// WorkspaceDir is the host checkout.
	WorkspaceDir string
	// Config is ReadMerged's result for WorkspaceDir (the create's preflight
	// read it); nil reads it here, which refuses a compose devcontainer too.
	Config *Config
	// Platform is the node platform to build for ("linux/arm64"), set as
	// DOCKER_DEFAULT_PLATFORM; "" builds for the daemon host's own.
	Platform string
	// Builder is the buildx builder to pin (BUILDX_BUILDER); "" pins the
	// current docker context's own builder, so a builder the user selected
	// with `docker buildx use` (a remote or docker-container one) is not
	// used: the image has to land in the local engine's store.
	Builder string
	// GPU answers the CLI's GPU detection: true when the cluster offers GPUs
	// (the CLI then adds --gpus all for a config that asks for one).
	GPU bool
	// NoCache builds without the layer cache (--build-no-cache).
	NoCache bool
	// Remote reaches the registry of an image-only configuration's image,
	// which is read from there when the daemon host lacks it for Platform;
	// the zero value uses the daemon host's docker logins.
	Remote kubeimage.RemoteOptions
}

// CaptureResult is a capture-up's outcome.
type CaptureResult struct {
	// Capture is the docker run the CLI asked for, secrets included; store
	// Capture.Redacted() and keep the environment values apart.
	Capture *kubeshim.Capture
	// Run is Capture parsed.
	Run *kubeshim.RunArgs
	// Image is the built (or pulled) image the run names.
	Image kubeshim.ImageConfig
	// Container is the container docker would have created (ContainerFor);
	// the pod's identity (ContainerID, Name, Created, StartedAt) is left for
	// the caller.
	Container kubeshim.Container
	// RemoteUser is the user devcontainer exec and the hooks run as.
	RemoteUser string
	// Config is the configuration the image was built from.
	Config *Config
	// CLIVersion is the devcontainer CLI's version.
	CLIVersion string
	// Calls is every docker call the CLI made, environment values redacted.
	Calls []kubeshim.Call
	// Warnings are the non-fatal problems found; each was also reported to
	// the job's provisioning log.
	Warnings []string
}

// CaptureUp builds the instance's image on the daemon host and captures the
// container the devcontainer CLI would run: `devcontainer up --docker-path
// <shim>` with a capture-mode shim, which hands the build to the real docker
// and records the final `docker run` instead of running it. The CLI keeps
// every devcontainer.json semantic (features, initializeCommand on the host,
// the start script, labels); the uid remap is left out
// (--update-remote-user-uid-default never), since the seed does it.
//
// Its log goes to ctx's provisioning log; a cancelled ctx kills the CLI and
// the builds it started.
func CaptureUp(ctx context.Context, req CaptureRequest) (*CaptureResult, error) {
	cfg := req.Config
	if cfg == nil {
		var err error
		cfg, err = ReadMerged(ctx, ReadRequest{Tools: req.Tools, WorkspaceDir: req.WorkspaceDir})
		if err != nil {
			return nil, err
		}
	}
	res := &CaptureResult{Config: cfg}
	warn := func(w string) {
		res.Warnings = append(res.Warnings, w)
		backend.ReportWarning(ctx, w)
	}
	for _, w := range BuildContextWarnings(cfg) {
		warn(w)
	}

	docker, err := req.docker()
	if err != nil {
		return nil, err
	}
	env, err := captureEnv(ctx, docker, req)
	if err != nil {
		return nil, err
	}
	env = append(env, originAgentEnv(cfg)...)
	dir, err := req.shimDir(kubeshim.Config{Mode: kubeshim.ModeCapture, Docker: docker})
	if err != nil {
		return nil, err
	}
	defer dir.Remove()

	args := []string{"up", "--workspace-folder", req.WorkspaceDir,
		"--docker-path", dir.Docker(), "--docker-compose-path", dir.Compose(),
		"--update-remote-user-uid-default", "never",
		"--gpu-availability", gpuAvailability(req.GPU)}
	if req.NoCache {
		args = append(args, "--build-no-cache")
	}
	jobLog := backend.JobLogFrom(ctx)
	r := req.run(ctx, args, env, jobLog)
	res.CLIVersion = cliVersion(r.log)
	res.Calls, _ = dir.Calls()

	capture, err := dir.Capture()
	if errors.Is(err, kubeshim.ErrNoCapture) {
		if composeCalled(dir) {
			return nil, &ComposeError{ConfigFile: cfg.ConfigFile}
		}
		if r.err == nil {
			r.err = errors.New("it finished without a docker run")
		}
		return nil, withRefusals(cliError("devcontainer up (image build)", r), res.Calls)
	}
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res.Capture = capture
	if res.Run, err = capture.Run(); err != nil {
		return nil, err
	}
	// One line, as a JSON array: word boundaries survive, and the log's own
	// redaction has nothing left to cut.
	argv, _ := json.Marshal(capture.Redacted().Args)
	fmt.Fprintf(jobLog, "captured: docker %s %s\n", capture.Verb, argv)

	if res.Image, err = imageFor(ctx, docker, res.Run.Image, req.Platform, req.Remote, jobLog); err != nil {
		return nil, err
	}
	if res.Container, err = kubeshim.ContainerFor(capture, res.Image); err != nil {
		return nil, err
	}
	res.RemoteUser = res.Container.RemoteUser()
	if !Tested(res.CLIVersion) {
		warn(untestedWarning(res.CLIVersion))
	}
	flog.Info("kubernetes capture-up", "workspace", req.WorkspaceDir, "image", res.Run.Image, "image_id", res.Image.ID, "cli", res.CLIVersion)
	return res, nil
}

// gpuAvailability is the CLI's --gpu-availability for a cluster that does
// (or does not) offer GPUs. It is always explicit, so the CLI never asks the
// daemon host's docker (`docker info`), whose GPUs are not the cluster's.
func gpuAvailability(gpu bool) string {
	if gpu {
		return "all"
	}
	return "none"
}

// captureEnv is the environment capture-up adds for the CLI and the builds it
// runs: the pinned builder, the node platform, and FLEET_DEVCONTAINER_BUILDKIT
// as the devcontainer backend applies it.
func captureEnv(ctx context.Context, docker string, req CaptureRequest) ([]string, error) {
	var env []string
	builder := req.Builder
	if builder == "" {
		builder = contextBuilder(ctx, docker)
	}
	if builder != "" {
		env = append(env, "BUILDX_BUILDER="+builder)
	}
	if req.Platform != "" {
		env = append(env, "DOCKER_DEFAULT_PLATFORM="+req.Platform)
	}
	switch mode := strings.TrimSpace(os.Getenv("FLEET_DEVCONTAINER_BUILDKIT")); mode {
	case "", "auto":
	case "never":
		if crossArch(req.Platform) {
			return nil, fmt.Errorf("building for %s on a %s host needs BuildKit, which FLEET_DEVCONTAINER_BUILDKIT=never turns off; unset it to build for this cluster", req.Platform, runtime.GOARCH)
		}
		env = append(env, "DOCKER_BUILDKIT=0")
	default:
		return nil, fmt.Errorf("invalid FLEET_DEVCONTAINER_BUILDKIT value %q (valid: auto, never)", mode)
	}
	return env, nil
}

// originAgentEnv points SSH_AUTH_SOCK back at the agent the daemon was
// started with for a configuration that names the daemon's relay socket
// (its ${localEnv:SSH_AUTH_SOCK}, read with fleetd's own environment, where
// the relay replaced it), as the devcontainer backend's up does: the captured
// run binds the user's own agent, and initializeCommand gets it too. Either
// path is a bind of the user's agent to the translation, which links it to
// the pod's agent relay.
func originAgentEnv(cfg *Config) []string {
	cur := agentsock.EnvAuthSock + "=" + os.Getenv(agentsock.EnvAuthSock)
	if cfg == nil || !bytes.Contains(cfg.Configuration, []byte(agentsock.HostSocketPath())) {
		return nil
	}
	if env := agentsock.WithOriginAgent([]string{cur}); env[0] != cur {
		return env
	}
	return nil
}

// contextBuilder is the name of the current docker context, which is also
// the name of that context's own (docker driver) buildx builder; "" when
// docker cannot say.
func contextBuilder(ctx context.Context, docker string) string {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, "context", "show")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		flog.Warn("kubernetes capture-up: docker context show failed; the build uses the selected buildx builder", "err", err)
		return ""
	}
	return strings.TrimSpace(out.String())
}

// crossArch reports whether platform ("linux/arm64", "linux/arm64/v8") is
// for another architecture than the daemon host's.
func crossArch(platform string) bool {
	if platform == "" {
		return false
	}
	parts := strings.Split(platform, "/")
	if len(parts) < 2 {
		return false
	}
	return parts[1] != runtime.GOARCH
}

// withRefusals adds the docker calls the shim refused to a capture-up
// failure: an unexpected call is the likely cause.
func withRefusals(err error, calls []kubeshim.Call) error {
	var refused []string
	for _, c := range calls {
		if c.Outcome == kubeshim.OutcomeRefused {
			refused = append(refused, fmt.Sprintf("`%s %s` (%s)", c.Binary, strings.Join(c.Args, " "), c.Detail))
		}
	}
	if len(refused) == 0 {
		return err
	}
	return fmt.Errorf("%w\nthe docker shim refused: %s", err, strings.Join(refused, "; "))
}
