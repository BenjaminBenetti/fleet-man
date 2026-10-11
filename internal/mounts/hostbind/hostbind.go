// Package hostbind names what a live bind of a daemon-host folder or file
// becomes where an instance cannot bind the daemon host's filesystem (a
// cluster pod): a folder of the fleet's share, kept in step with the host
// path by the fleet's sync. The host path stays where it is (~/.aws, a
// custom mount's chosen host folder); its copy on the share is
// .hostbind/<id>, the id being a hash of the cleaned host path, so every
// instance that binds one host path shares one copy, as every container
// binding it shares the host's folder on docker.
//
// A bound folder's copy is the folder itself. A bound file's copy is a
// folder holding just that file, under its own name: the pod mounts the
// folder at ContainerDir/<id> and links the bind's target to the file in
// it, so a write that replaces the file (a rename over it) works in the
// pod, where a bind of the file alone would pin the old one.
package hostbind

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"strings"
)

// Root is the share folder every host bind's copy is in, and the policy
// table's root that covers them (internal/mounts/syncpolicy).
const Root = ".hostbind"

// ContainerDir is where a pod mounts the folders holding bound files.
const ContainerDir = "/fleet-mounts/hostbind"

// ID is a host path's id: the first 8 hex digits of the SHA-256 of the
// path, cleaned.
func ID(hostPath string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(hostPath)))
	return hex.EncodeToString(sum[:4])
}

// Name is the share folder of hostPath's copy, relative to the fleet's
// tree: .hostbind/<id>. It is also the sync root that keeps it.
func Name(hostPath string) string { return Root + "/" + ID(hostPath) }

// FileDir is where a pod mounts the folder holding the bound file whose
// copy is name (Name's result).
func FileDir(name string) string { return path.Join(ContainerDir, strings.TrimPrefix(name, Root+"/")) }

// Is reports whether name is a host bind's copy (Name's form).
func Is(name string) bool {
	id, ok := strings.CutPrefix(name, Root+"/")
	if !ok || len(id) != 8 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Overlaps reports whether paths a and b are one folder or one holds the
// other (both absolute, compared cleaned).
func Overlaps(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || within(a, b) || within(b, a)
}

// within reports whether p is below dir.
func within(p, dir string) bool {
	if dir == string(filepath.Separator) {
		return p != dir
	}
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}
