package kubernetes

import (
	"path/filepath"
	"slices"

	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// ShareRecord is where an instance's fleet mounts live on its connection,
// as its record keeps it: what attaching its share's spoke takes (the
// daemon's sync manager reads it).
type ShareRecord struct {
	// Mode is the share's mode (topology.Mode).
	Mode string `json:"mode"`
	// Claim is the fleet's share PVC ("" on the copy tier, where the copy is
	// the instance's own).
	Claim string `json:"claim,omitempty"`
	// Roots are the tree's synced folders, Layout paths reserved.
	Roots []podshare.SpokeRoot `json:"roots"`
	// UID and GID are the share uid (-1 in a pod without root).
	UID int `json:"uid"`
	GID int `json:"gid"`
	// Volumes are the fleet's named volumes the instance mounts, by their
	// folder in the share's volumes folder (podshare.VolumeSubPath).
	Volumes []string `json:"volumes,omitempty"`
	// HostBinds are the daemon-host folders and files the instance binds,
	// whose copies are on the share: each one of Roots, kept in step with
	// its host path by the daemon's sync.
	HostBinds []HostBind `json:"hostBinds,omitempty"`
}

// HostBind is a daemon-host folder or file an instance binds (a
// devcontainer's bind mount, a custom mount's chosen host folder), whose
// copy is on its share.
type HostBind struct {
	// Name is the copy's folder on the share, and its synced root
	// (hostbind.Name of Path).
	Name string `json:"name"`
	// Path is the bound folder or file on the daemon host.
	Path string `json:"path"`
	// File: Path is a file, the one name its copy's folder holds.
	File bool `json:"file,omitempty"`
	// ReadOnly: the instance mounts it read-only alone, so its copy only
	// follows Path (host to pod).
	ReadOnly bool `json:"readOnly,omitempty"`
}

// root is the synced root of the bind's copy.
func (b HostBind) root() podshare.SpokeRoot {
	r := podshare.SpokeRoot{Name: b.Name}
	if b.File {
		r.Only = []string{filepath.Base(b.Path)}
	}
	return r
}

// SpokeSpec is what the instance's pod's spoke runs: its tree's folders,
// as the share uid.
func (r ShareRecord) SpokeSpec(pod string) podshare.SpokeSpec {
	return podshare.SpokeSpec{V: podshare.SpokeVersion, Roots: r.Roots, UID: r.UID, GID: r.GID, Pod: pod}
}

// clone is a copy of r sharing nothing with it (nil for nil): a clone's pod
// mounts its source's share (or its own copy of the source's tree) alike.
func (r *ShareRecord) clone() *ShareRecord {
	if r == nil {
		return nil
	}
	out := *r
	out.Roots = make([]podshare.SpokeRoot, len(r.Roots))
	for i, root := range r.Roots {
		out.Roots[i] = podshare.SpokeRoot{Name: root.Name, Reserved: slices.Clone(root.Reserved), Skip: slices.Clone(root.Skip), Only: slices.Clone(root.Only)}
	}
	out.Volumes = slices.Clone(r.Volumes)
	out.HostBinds = slices.Clone(r.HostBinds)
	return &out
}
