package server

import (
	"context"
	"errors"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	devcontainercheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/devcontainer"
	homedircheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/homedir"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inspect.go implements the InspectRepo RPC: shallow-clone a fleet's remote on
// THIS host and run the inspector checks against it. Inspection must happen
// where provisioning happens — the daemon's host owns the git credentials and
// docker daemon that `fleet up` will actually use — so a remote TUI gets the
// same verdict provisioning would, instead of one biased by the client
// machine's credentials (issue #141 note 5).

// inspectorOpen is the package-var seam over inspector.Open so tests can hand
// the handler a prepared Repo without a network clone (the openForwardBridge
// precedent in forward.go).
var inspectorOpen = inspector.Open

// InspectRepo shallow-clones remote_url (optionally at branch), reports
// whether the repo declares a devcontainer config, and — when detect_home_dir
// is set — best-effort guesses the container user's home directory.
//
// inspector.Open bounds the clone with its own 90s timeout; the home-dir
// check may shell out to docker (and pull the devcontainer image) — that is
// the point: it runs against the daemon host's docker, the one provisioning
// will use. Clients must therefore call this with a generous deadline.
func (s *service) InspectRepo(_ context.Context, req *fleetgrpc.InspectRepoRequest) (*fleetgrpc.InspectRepoReply, error) {
	remoteURL := req.GetRemoteUrl()
	if remoteURL == "" {
		return nil, status.Error(codes.InvalidArgument, "remote_url is required")
	}

	repo, err := inspectorOpen(remoteURL, req.GetBranch())
	if err != nil {
		// FailedPrecondition rather than Internal: the clone error text
		// (unreachable host, bad credentials, unknown branch) is the user-facing
		// diagnosis — the TUI surfaces it verbatim so the user can fix the URL.
		flog.Warn("inspect: clone failed", "remote", remoteURL, "branch", req.GetBranch(), "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	defer repo.Close()

	// (false, nil) is the normal "repo has no devcontainer" answer, not an
	// error; only a real I/O failure reading the clone surfaces as Internal.
	hasDevcontainer, err := devcontainercheck.Present(repo)
	if err != nil {
		flog.Warn("inspect: devcontainer check failed", "remote", remoteURL, "err", err)
		return nil, status.Errorf(codes.Internal, "devcontainer check: %v", err)
	}

	reply := &fleetgrpc.InspectRepoReply{HasDevcontainer: hasDevcontainer}
	if req.GetDetectHomeDir() {
		// Home-dir detection is best-effort by design: "no hint" (no
		// devcontainer.json, no remoteUser/containerUser, image unreadable) is an
		// expected outcome, and even unexpected failures must not fail the RPC —
		// an empty home_dir tells the caller to fall back to manual entry.
		homeDir, err := homedircheck.Detect(repo)
		switch {
		case err == nil:
			reply.HomeDir = homeDir
		case errors.Is(err, homedircheck.ErrNoUserHint), errors.Is(err, inspector.ErrNoDevcontainerConfig):
			// Expected: the repo simply offers no hint.
		default:
			flog.Warn("inspect: homedir detect failed", "remote", remoteURL, "err", err)
		}
	}

	flog.Info("inspect: repo inspected", "remote", remoteURL, "branch", req.GetBranch(),
		"hasDevcontainer", reply.GetHasDevcontainer(), "homeDir", reply.GetHomeDir())
	return reply, nil
}
