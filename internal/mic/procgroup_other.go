//go:build !unix

package mic

import (
	"context"
	"os/exec"
)

// guardedCommand runs argv directly where there are no unix process groups;
// cancelling kills the direct child only.
func guardedCommand(ctx context.Context, argv ...string) (*exec.Cmd, func(), error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...), func() {}, nil
}
