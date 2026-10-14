package devcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// LayerRequest asks for one layer built on the daemon host over an image
// capture-up built: a step that needs root at build time, for a cluster whose
// containers never get it (the audio stack on a cluster without root).
type LayerRequest struct {
	Tools
	// From is the image to build on, by the reference its build tagged
	// (capture-up's RunArgs.Image); Base is its config (whose user the layer
	// restores after its root step).
	From string
	Base kubeshim.ImageConfig
	// Platform and Builder are capture-up's (CaptureRequest).
	Platform string
	Builder  string
	// Files are put in the build context and copied to LayerDir, each by its
	// name (a plain file name).
	Files map[string]string
	// Run is the shell command the layer runs as root, in LayerDir; LayerDir
	// is removed after it.
	Run string
}

// LayerDir is where a layer's files are while its step runs.
const LayerDir = "/tmp/fleet-layer"

// layerFile is a name a layer file may have.
var layerFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LayerDockerfile is the Dockerfile of req's layer: its files copied in, its
// step run as root, its files removed, the image's user restored.
func LayerDockerfile(req LayerRequest) (string, error) {
	if req.From == "" || strings.ContainsAny(req.From, " \t\r\n") {
		return "", fmt.Errorf("layer base %q is not an image reference", req.From)
	}
	if strings.ContainsAny(req.Run, "\r\n") || req.Run == "" {
		return "", fmt.Errorf("a layer runs one command line")
	}
	user := req.Base.Config.User
	if strings.ContainsAny(user, " \t\r\n") {
		return "", fmt.Errorf("the image's user %q cannot be restored", user)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\nUSER root\n", req.From)
	names := make([]string, 0, len(req.Files))
	for name := range req.Files {
		if !layerFile.MatchString(name) {
			return "", fmt.Errorf("layer file %q is not a plain file name", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "COPY %s %s/%s\n", name, LayerDir, name)
	}
	fmt.Fprintf(&b, "RUN cd %s && %s && cd / && rm -rf %s\n", LayerDir, req.Run, LayerDir)
	if user != "" {
		fmt.Fprintf(&b, "USER %s\n", user)
	}
	return b.String(), nil
}

// AddLayer builds req's layer with the daemon host's docker (the builder and
// platform pinned as capture-up pins them) and returns the new image's
// config. The image is left untagged: its delivery addresses it by id (and
// tags it, for a node load or a push). The build's output goes to ctx's
// provisioning log.
func AddLayer(ctx context.Context, req LayerRequest) (kubeshim.ImageConfig, error) {
	dockerfile, err := LayerDockerfile(req)
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	docker, err := req.docker()
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	env, err := captureEnv(ctx, docker, CaptureRequest{Platform: req.Platform, Builder: req.Builder})
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	dir, err := os.MkdirTemp("", "fleet-layer-*")
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	defer os.RemoveAll(dir)
	buildDir := filepath.Join(dir, "context")
	if err := os.Mkdir(buildDir, 0o700); err != nil {
		return kubeshim.ImageConfig{}, err
	}
	for name, content := range req.Files {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte(content), 0o644); err != nil {
			return kubeshim.ImageConfig{}, err
		}
	}
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte(dockerfile), 0o644); err != nil {
		return kubeshim.ImageConfig{}, err
	}
	iid := filepath.Join(dir, "iid")
	jobLog := backend.JobLogFrom(ctx)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, "build", "--iidfile", iid, "-f", df, buildDir)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = jobLog
	cmd.Stderr = io.MultiWriter(jobLog, &tail{buf: &stderr, max: 4096})
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return kubeshim.ImageConfig{}, ctx.Err()
		}
		return kubeshim.ImageConfig{}, fmt.Errorf("docker build: %w: %s", err, lastLines(stderr.String(), 3))
	}
	raw, err := os.ReadFile(iid)
	if err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("docker build left no image id: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	img, err := inspectImage(ctx, docker, id, req.Platform)
	if err != nil {
		return kubeshim.ImageConfig{}, err
	}
	if img.ID == "" {
		img.ID = id
	}
	return img, nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	buf *bytes.Buffer
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - t.max; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

// lastLines is s's last n non-empty lines, joined by "; ".
func lastLines(s string, n int) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
