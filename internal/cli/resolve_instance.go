package cli

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
)

// resolveInstance is the shared lookup used by commands that target a single
// instance. It resolves a user-provided name (the "instance", "fleet/instance",
// or repo-derived form handled by fleet.Resolve) and confirms the instance
// exists, returning the resolved target and the instance record.
//
// The repoFlag is forwarded to fleet.Resolve; pass "" for the common case where
// the fleet is inferred from the name or cwd. (instanceops.LoadInstance reads
// state.json directly; that's fine here — instanceops is not client-boundary
// code. Commands that MUTATE go through the server's job RPCs instead.)
func resolveInstance(name, repoFlag string) (*fleet.Target, *fleet.Instance, error) {
	target, err := fleet.Resolve(name, repoFlag)
	if err != nil {
		return nil, nil, err
	}

	_, _, instance, err := instanceops.LoadInstance(target.Fleet, target.Instance)
	if err != nil {
		return nil, nil, err
	}

	return target, instance, nil
}
