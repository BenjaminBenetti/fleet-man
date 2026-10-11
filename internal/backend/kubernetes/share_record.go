package kubernetes

import (
	"slices"

	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// ShareRecord is where an instance's fleet mounts live on its connection,
// as its record keeps it: what attaching its share's spoke takes (P4.6).
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
		out.Roots[i] = podshare.SpokeRoot{Name: root.Name, Reserved: slices.Clone(root.Reserved), Skip: slices.Clone(root.Skip)}
	}
	return &out
}
