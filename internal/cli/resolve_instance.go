package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// resolveInstance is the shared lookup used by every command that targets a
// single instance. It resolves a user-provided name (the "instance",
// "fleet/instance", or repo-derived form handled by fleet.Resolve), loads
// state, and returns the resolved target along with the loaded state, the
// owning fleet, and the instance.
//
// The repoFlag is forwarded to fleet.Resolve; pass "" for the common case
// where the fleet is inferred from the name or cwd. The returned state and
// fleet are provided for callers that go on to mutate and persist state
// (e.g. removing the instance); callers that only read the instance can
// ignore them.
func resolveInstance(name, repoFlag string) (*fleet.Target, *state.State, *fleet.Fleet, *fleet.Instance, error) {
	target, err := fleet.Resolve(name, repoFlag)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	st, f, instance, err := instanceops.LoadInstance(target.Fleet, target.Instance)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return target, st, f, instance, nil
}
