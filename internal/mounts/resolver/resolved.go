package resolver

import "github.com/BenjaminBenetti/fleet-man/internal/backend"

// Resolved bundles the artefacts a fleet's mount settings translate
// into: zero or more bind mounts (passed to backend.Up) plus zero or
// more symlinks the caller must create inside the container after the
// mounts are in place.
//
// Splitting Mounts from Symlinks lets the backend interface stay
// generic: backends only know how to set up bind mounts, while the
// symlink step lives in the create pipeline where the backend's
// Exec capability can be used uniformly across implementations.
type Resolved struct {
	// Mounts is the slice of bind mounts to pass to backend.Up.
	Mounts []backend.Mount
	// Symlinks is the slice of symbolic links to create after the
	// mounts are in place — typically used for single-file shares
	// where a direct bind mount would break atomic-write semantics.
	Symlinks []Symlink
}
