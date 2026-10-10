package binstore

import (
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// VersionSymbol is the variable the release builds stamp fleet's version
// into (`-ldflags -X VersionSymbol=<tag>`, see the Makefile's LDFLAGS).
const VersionSymbol = "github.com/BenjaminBenetti/fleet-man/internal/version.Version"

// Identity is which build of fleet's source a binary is: its release version,
// or for a dev build (no version) the commit it was built from.
type Identity struct {
	// Version is the release tag the build was stamped with; "" for a dev
	// build.
	Version string
	// Revision and Modified are the VCS commit the build was made from and
	// whether the tree had changes, as `go build` records them; empty when it
	// recorded none (-buildvcs=false, a test binary).
	Revision string
	Modified bool
}

// Equal reports whether a and b are the same build of fleet's source: the
// same release version, or (two dev builds) the same commit in the same
// state. Two dev builds that recorded no commit cannot be told apart and
// count as equal.
func (a Identity) Equal(b Identity) bool {
	if a.Version != "" || b.Version != "" {
		return a.Version == b.Version
	}
	return a.Revision == b.Revision && a.Modified == b.Modified
}

func (a Identity) String() string {
	switch {
	case a.Version != "":
		return a.Version
	case a.Revision == "":
		return "a dev build"
	}
	rev := a.Revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if a.Modified {
		rev += "+changes"
	}
	return "dev build " + rev
}

// Self is the running fleet's identity.
func Self() Identity {
	id := Identity{Version: version.Version}
	if info, ok := debug.ReadBuildInfo(); ok {
		id.Revision, id.Modified = vcs(info)
	}
	return id
}

// Info is what a fleet binary's embedded build information says about it.
type Info struct {
	Identity
	// Flavor is the build: the full one fleetd runs, or the pod build
	// (-tags nok8s) staged into pods.
	Flavor buildflavor.Flavor
	// GOOS and GOARCH are its target.
	GOOS, GOARCH string
}

// Inspect reads the build information of the Go binary at path, built for
// any OS or arch.
func Inspect(path string) (Info, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return Info{}, fmt.Errorf("%s is not a Go binary with build information: %w", path, err)
	}
	info := Info{Flavor: buildflavor.Of(bi)}
	info.Revision, info.Modified = vcs(bi)
	for _, s := range bi.Settings {
		switch s.Key {
		case "GOOS":
			info.GOOS = s.Value
		case "GOARCH":
			info.GOARCH = s.Value
		case "-ldflags":
			info.Version = stampedVersion(s.Value)
		}
	}
	return info, nil
}

func vcs(bi *debug.BuildInfo) (revision string, modified bool) {
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return revision, modified
}

// stampedVersion is the version an -ldflags string stamps (`-X
// VersionSymbol=<v>`, the last one winning as with the linker); "" without
// one.
func stampedVersion(ldflags string) string {
	fields := strings.Fields(ldflags)
	v := ""
	for i, f := range fields {
		arg := ""
		switch {
		case f == "-X" && i+1 < len(fields):
			arg = fields[i+1]
		case strings.HasPrefix(f, "-X="):
			arg = strings.TrimPrefix(f, "-X=")
		default:
			continue
		}
		arg = strings.Trim(arg, `'"`)
		if value, ok := strings.CutPrefix(arg, VersionSymbol+"="); ok {
			v = value
		}
	}
	return v
}
