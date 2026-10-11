package podshare

import (
	"path"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// Where the tree is in an instance pod's containers, and its layout.
const (
	// SyncDir holds MountDir: a folder of the pod's run directory only
	// root may enter.
	SyncDir = podseed.RunDir + "/sync"
	// MountDir is where the tree's root mounts: the fleet's share volume,
	// or on the copy tier the instance disk's DiskDir.
	MountDir = SyncDir + "/share"
	// DiskDir is the instance disk's folder holding the tree on the copy
	// tier (its sync/).
	DiskDir = podseed.DirSync

	// RootsDir holds the fleet's folders.
	RootsDir = "roots"
	// StateDir holds the spoke's manifests.
	StateDir = "state"
	// LeaseFile is the spoke's lease.
	LeaseFile = "lease"

	// LayoutFile is the layout's name in the instance's spec Secret, which
	// the pod mounts at podseed.SpecDir.
	LayoutFile = "layout.json"
	// SpokeDirName is the spoke's private folder in the agents' run
	// directory, where its socket is.
	SpokeDirName = "spoke"
	// SpokeSocketName is the spoke's socket in that folder.
	SpokeSocketName = "spoke.sock"
)

// SubPath is the tree's path of the fleet folder at sub (a fleet mount's
// SubPath: ".claude", "files", ".mnt/opt/data"): roots/<sub>.
func SubPath(sub string) string { return path.Join(RootsDir, sub) }

// LayoutPath is where the init container reads the layout.
var LayoutPath = path.Join(podseed.SpecDir, LayoutFile)
