package kubeimage

import (
	"fmt"
	"strings"
)

// Names of what fleet creates for an image. Every one carries the daemon
// (its first 8 id characters, daemon8), so the GC only ever touches its own.

// repoName is the last component of every repository fleet pushes to.
const repoName = "devcontainer"

// nodeLoadDomain is the registry of the tags node load creates. ".invalid"
// never resolves (RFC 2606), so a pod that tried to pull one by mistake fails
// at once instead of asking Docker Hub.
const nodeLoadDomain = "fleet.invalid"

// daemon8 is the daemon's short id in repository paths.
func daemon8(daemonID string) string {
	id := strings.ToLower(daemonID)
	return id[:min(8, len(id))]
}

// owner12 is the daemon's owner label value (the first 12 id characters,
// lowercased; the same as names.Owner).
func owner12(daemonID string) string {
	id := strings.ToLower(daemonID)
	return id[:min(12, len(id))]
}

// id12 is the short image id fleet tags an image with: the first 12 hex
// digits of its daemon-host image ID.
func id12(imageID string) string {
	hex := strings.TrimPrefix(imageID, "sha256:")
	return hex[:min(12, len(hex))]
}

// ownPath is the repository path, under a registry's prefix, of the daemon's
// images: "<prefix>/fleet/<daemon8>/devcontainer".
func ownPath(prefix, daemonID string) string {
	p := "fleet/" + daemon8(daemonID) + "/" + repoName
	if prefix != "" {
		p = prefix + "/" + p
	}
	return p
}

// RegistryRepository is the repository the daemon pushes a cluster's images
// to: "<registry>/fleet/<daemon8>/devcontainer".
func RegistryRepository(reg Registry, daemonID string) string {
	return reg.Domain + "/" + ownPath(reg.Prefix, daemonID)
}

// RegistryTag is the tag an image is pushed as:
// "<registry>/fleet/<daemon8>/devcontainer:<id12>".
func RegistryTag(reg Registry, daemonID, imageID string) string {
	return RegistryRepository(reg, daemonID) + ":" + id12(imageID)
}

// NodeLoadTag is the tag node load gives an image on the daemon host and on
// the nodes: "fleet.invalid/<daemon8>/devcontainer:<id12>".
func NodeLoadTag(daemonID, imageID string) string {
	return nodeLoadDomain + "/" + daemon8(daemonID) + "/" + repoName + ":" + id12(imageID)
}

// isOwnRepository reports whether repo (a full repository, registry
// included) is one of the daemon's: its path ends in
// "fleet/<daemon8>/devcontainer" at a component boundary. The GC deletes
// nothing anywhere else.
func isOwnRepository(repo, daemonID string) bool {
	_, path, ok := strings.Cut(repo, "/")
	if !ok || len(daemonID) < 8 {
		return false
	}
	own := ownPath("", daemonID)
	return path == own || strings.HasSuffix(path, "/"+own)
}

// isOwnTag reports whether tag is one fleet gives imageID for the daemon:
// its node load tag, or a tag in one of its registry repositories. The GC
// removes no other tag.
func isOwnTag(tag, daemonID, imageID string) bool {
	r, err := ParseReference(tag)
	if err != nil || r.Digest != "" || r.Tag != id12(imageID) || len(daemonID) < 8 {
		return false
	}
	if r.Domain == nodeLoadDomain {
		return r.Path == daemon8(daemonID)+"/"+repoName
	}
	return isOwnRepository(r.Repository(), daemonID)
}

// PullSecretName is the name of the managed pull Secret of one connection:
// "fleet-pull-<owner12>-<cluster id>". It names the connection as well as the
// daemon (like the owner Lease), so two connections to one namespace each
// keep their own.
func PullSecretName(daemonID, clusterID string) string {
	return "fleet-pull-" + owner12(daemonID) + "-" + clusterID
}

// validateDaemonID refuses an id too short to name the daemon's objects.
func validateDaemonID(daemonID string) error {
	if len(daemonID) < 12 {
		return fmt.Errorf("daemon id %q is too short", daemonID)
	}
	return nil
}
