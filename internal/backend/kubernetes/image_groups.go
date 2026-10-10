//go:build !nok8s

package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// image_groups.go resolves a runArgs `--group-add <name>` the way docker
// does when it starts the container: against the image's /etc/group. A pod's
// supplementalGroups are numbers, so the create reads the built image's
// /etc/group from the daemon host's docker, the first time a name needs it:
// a container created (never started, no network, never pulled) and the file
// copied out of it.

// imageGroupsTimeout bounds reading an image's /etc/group.
const imageGroupsTimeout = time.Minute

// imageGroups is one image's /etc/group, read once.
type imageGroups struct {
	docker, image, platform string
	// report is told why the file could not be read.
	report func(error)

	once   sync.Once
	groups map[string]int64
}

// lookup is podspec.Input.LookupGroup for the image: the gid of the first
// entry named name.
func (g *imageGroups) lookup(ctx context.Context) func(name string) (int64, bool) {
	return func(name string) (int64, bool) {
		g.once.Do(func() {
			groups, err := readImageGroups(ctx, g.docker, g.image, g.platform)
			if err != nil && g.report != nil {
				g.report(err)
			}
			g.groups = groups
		})
		gid, ok := g.groups[name]
		return gid, ok
	}
}

// readImageGroups reads image's /etc/group (platform's variant when set):
// group name to gid, the first entry of a name winning, as docker's lookup
// does.
func readImageGroups(ctx context.Context, docker, image, platform string) (map[string]int64, error) {
	if image == "" {
		return nil, errors.New("the image's /etc/group: no image")
	}
	ctx, cancel := context.WithTimeout(ctx, imageGroupsTimeout)
	defer cancel()
	args := []string{"create", "--pull=never", "--network=none", "--entrypoint=/fleet-never-runs"}
	if platform != "" {
		args = append(args, "--platform="+platform)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, append(args, image)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("the image's /etc/group: docker create: %v%s", err, detail(stderr.String()))
	}
	id := strings.TrimSpace(string(out))
	defer func() {
		rm, cancel := context.WithTimeout(context.Background(), imageGroupsTimeout)
		defer cancel()
		_ = exec.CommandContext(rm, docker, "rm", "-f", id).Run()
	}()

	stderr.Reset()
	cmd = exec.CommandContext(ctx, docker, "cp", id+":/etc/group", "-")
	cmd.Stderr = &stderr
	stream, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("the image's /etc/group: docker cp: %v%s", err, detail(stderr.String()))
	}
	return parseGroupTar(bytes.NewReader(stream))
}

// parseGroupTar reads the /etc/group that `docker cp <container>:/etc/group -`
// streams, one file in a tar.
func parseGroupTar(r io.Reader) (map[string]int64, error) {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("the image's /etc/group: %w", err)
	}
	if h.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("the image's /etc/group is not a regular file")
	}
	groups, err := execas.ParseGroup(tr)
	if err != nil {
		return nil, fmt.Errorf("the image's /etc/group: %w", err)
	}
	byName := make(map[string]int64, len(groups))
	for _, g := range groups {
		if _, seen := byName[g.Name]; !seen {
			byName[g.Name] = int64(g.GID)
		}
	}
	return byName, nil
}
