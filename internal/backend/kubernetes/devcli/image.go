package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// imageFor reads the config of the image a captured run names. When the
// daemon host does not have it (for platform), it is read from its registry
// instead: an image-only configuration is never built, and the docker run the
// CLI asked for, which would have pulled it, was captured instead. Only if
// the registry cannot say is the image pulled: on docker's classic store a
// pull for another architecture moves the host's tag to that architecture,
// under the devcontainer-backend instances that use it. The pull's output
// goes to jobLog.
func imageFor(ctx context.Context, docker, image, platform string, remote kubeimage.RemoteOptions, jobLog io.Writer) (kubeshim.ImageConfig, error) {
	img, inspectErr := inspectImage(ctx, docker, image, platform)
	if inspectErr == nil {
		return img, nil
	}
	img, err := kubeimage.RemoteImageConfig(ctx, image, platform, remote)
	if err == nil {
		fmt.Fprintf(jobLog, "read %s from its registry (the daemon host has no image for this cluster's platform)\n", image)
		return img, nil
	}
	if ctx.Err() != nil {
		return kubeshim.ImageConfig{}, ctx.Err()
	}
	flog.Info("kubernetes capture-up: reading the image from its registry failed; pulling it", "image", image, "err", err)
	args := []string{"pull"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(args, "--", image)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Stdout = jobLog
	cmd.Stderr = io.MultiWriter(&stderr, jobLog)
	if err := cmd.Run(); err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("%w; docker pull %s: %v: %s", inspectErr, image, err, strings.TrimSpace(stderr.String()))
	}
	return inspectImage(ctx, docker, image, platform)
}

// inspectImage reads image's config from the real docker. With a platform
// it asks for that platform's image, which a containerd image store needs
// when the build was for another architecture (docker 28+); an older docker
// without the option is asked without it.
//
// The id it returns is the one docker addresses the image by (what `docker
// tag`, `docker save` and a push take): on a containerd image store a
// platform's inspect reports the id of that platform's manifest, which docker
// does not address, so the image's own id (its index's) is asked for
// separately; the config stays the platform's.
func inspectImage(ctx context.Context, docker, image, platform string) (kubeshim.ImageConfig, error) {
	img, err := dockerImageInspect(ctx, docker, image, platform)
	if err != nil && platform != "" && strings.Contains(err.Error(), "unknown flag") {
		return dockerImageInspect(ctx, docker, image, "")
	}
	if err != nil || platform == "" {
		return img, err
	}
	if own, err := dockerImageInspect(ctx, docker, image, ""); err == nil && own.ID != "" && own.ID != img.ID {
		img.ID = own.ID
	}
	return img, nil
}

func dockerImageInspect(ctx context.Context, docker, image, platform string) (kubeshim.ImageConfig, error) {
	args := []string{"image", "inspect"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(args, "--", image)
	cmd := exec.CommandContext(ctx, docker, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("docker image inspect %s: %w: %s", image, err, strings.TrimSpace(stderr.String()))
	}
	var imgs []kubeshim.ImageConfig
	if err := json.Unmarshal(stdout.Bytes(), &imgs); err != nil {
		return kubeshim.ImageConfig{}, fmt.Errorf("parsing docker image inspect %s: %w", image, err)
	}
	if len(imgs) != 1 {
		return kubeshim.ImageConfig{}, fmt.Errorf("docker image inspect %s returned %d images", image, len(imgs))
	}
	return imgs[0], nil
}
