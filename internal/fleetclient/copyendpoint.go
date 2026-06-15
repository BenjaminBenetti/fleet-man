package fleetclient

import (
	"slices"
	"strings"
)

// copyendpoint.go classifies the two arguments of a scp-style `fleet copy`. An
// endpoint is one of three kinds; direction is inferred from which side is local
// vs an instance. The parser is shared by the CLI (host form) and is mirrored by
// the in-instance form, so a path means the same thing everywhere.

// CopyEndpointKind classifies one side of a copy.
type CopyEndpointKind int

const (
	// CopyLocal is a plain path on the orchestrating client's own disk.
	CopyLocal CopyEndpointKind = iota
	// CopyInstance is `[fleet/]instance:path` — a path inside a named instance.
	CopyInstance
	// CopySelf is `:path` — a path inside the CURRENT instance. Only meaningful
	// in the in-instance form, where the originating instance is known; the host
	// form rejects it (there is no "current" instance).
	CopySelf
)

// CopyEndpoint is one parsed side of a copy.
type CopyEndpoint struct {
	Kind     CopyEndpointKind
	Fleet    string // set for CopyInstance when a fleet was named ("fleet/instance")
	Instance string // set for CopyInstance
	Path     string // the path component (the whole arg for CopyLocal)
}

// ParseCopyEndpoint classifies a `fleet copy` argument, scp-style:
//
//   - a leading '/', '.' or '~' always forces a LOCAL path (so a local filename
//     containing a colon is still reachable, spelled "./name:with:colon"),
//   - ":path" (empty prefix, non-empty path) is the CURRENT instance (SELF),
//   - "[fleet/]instance:path" is a named INSTANCE,
//   - anything else — including a malformed instance ref or an empty path after
//     the colon — is treated as LOCAL, so the caller's existence check yields a
//     plain "no such file" rather than a confusing parse error.
func ParseCopyEndpoint(arg string) CopyEndpoint {
	i := strings.Index(arg, ":")
	if i < 0 || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return CopyEndpoint{Kind: CopyLocal, Path: arg}
	}
	path := arg[i+1:]
	if path == "" {
		return CopyEndpoint{Kind: CopyLocal, Path: arg}
	}
	if i == 0 {
		return CopyEndpoint{Kind: CopySelf, Path: path}
	}
	parts := strings.Split(arg[:i], "/")
	if len(parts) > 2 || slices.Contains(parts, "") {
		return CopyEndpoint{Kind: CopyLocal, Path: arg}
	}
	if len(parts) == 2 {
		return CopyEndpoint{Kind: CopyInstance, Fleet: parts[0], Instance: parts[1], Path: path}
	}
	return CopyEndpoint{Kind: CopyInstance, Instance: parts[0], Path: path}
}
