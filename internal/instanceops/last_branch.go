package instanceops

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// stopBranchReadTimeout bounds the branch read a stop makes first, so a
// wedged workspace read can't hold the stop up.
const stopBranchReadTimeout = 5 * time.Second

// readWorkspaceBranch reads the branch checked out in instance's workspace
// through its backend's WorkspaceReader. It is a package var so tests can
// stub it.
var readWorkspaceBranch = func(instance *fleet.Instance) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), stopBranchReadTimeout)
	defer cancel()
	reader := backendutil.WorkspaceReaderFor(backendutil.NewForInstance(instance, false))
	return reader.Branch(ctx, instance.WorkspaceDir)
}

// recordLastBranch sets instance.LastBranch to the branch its workspace is on,
// read while the instance still runs: a backend whose workspace lives only in
// the running instance can't be read once it stops, and every branch display
// then falls back to LastBranch. A failed read or a detached HEAD keeps the
// previous value.
func recordLastBranch(instance *fleet.Instance) {
	if branch, ok := readWorkspaceBranch(instance); ok && branch != "" {
		instance.LastBranch = branch
	}
}
