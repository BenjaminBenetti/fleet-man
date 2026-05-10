// Package devcontainer is an inspector check: given an *inspector.Repo
// it reports whether the repository contains a devcontainer
// configuration that fleet-man can provision.
//
// The actual file-system search is delegated to
// inspector.Repo.FindDevcontainerJSON so this package stays a thin
// boolean wrapper — new checks can stack on top of the same Repo handle
// without each having to reimplement the lookup.
package devcontainer

import (
	"errors"

	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
)

// Present reports whether repo declares a devcontainer.json in any of
// the locations recognised by the devcontainer CLI.
//
// A missing file returns (false, nil) — that is an expected, non-error
// state that the caller surfaces to the user. Any other I/O failure is
// returned as-is so the caller can decide whether to retry or fall
// back.
func Present(repo *inspector.Repo) (bool, error) {
	_, _, err := repo.FindDevcontainerJSON()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, inspector.ErrNoDevcontainerConfig) {
		return false, nil
	}
	return false, err
}
