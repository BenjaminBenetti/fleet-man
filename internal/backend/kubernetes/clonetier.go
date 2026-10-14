package kubernetes

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// clonetier.go chooses how a clone's disk is made (selectCloneTier), from
// what the source disk's storage offers and the connection's clone settings:
//
//	T1  snapshot   a VolumeSnapshotClass serves the disk's CSI driver: the
//	               source's disk is flushed (and its user processes frozen,
//	               where the pod can be) and snapshotted at one point in time,
//	               and the clone's disk restored from the snapshot.
//	T1b csi-clone  no snapshot class, the source is stopped and its driver
//	               clones volumes: the clone's disk is a CSI clone of the
//	               source's.
//	T2  copy       any disk: a copier Job beside the source copies it live in
//	               passes, then takes the final delta while the source is held
//	               still (frozen, or stopped with clone_consistency
//	               stop-source), or, where the source cannot be frozen,
//	               copies it live as well, and the clone is best-effort.
//
// A stopped source needs no holding: whatever tier copies it, the copy is of
// a disk nothing writes. A tier the settings name that the cluster cannot do
// fails the clone, naming the setting, rather than falling back silently.

// CloneTier is how a clone's disk is made.
type CloneTier string

const (
	TierSnapshot CloneTier = "snapshot"
	TierCSIClone CloneTier = "csi-clone"
	TierCopy     CloneTier = "copy"
)

// CloneHold is how the source is held still for the point a consistent clone
// is taken at: a snapshot's cut, or a copier's final delta.
type CloneHold string

const (
	// HoldNone: the source is stopped, so nothing writes its disk.
	HoldNone CloneHold = "none"
	// HoldFreeze freezes the source's user processes (a privileged pod's
	// user cgroup); a pod that cannot be frozen is copied live.
	HoldFreeze CloneHold = "freeze"
	// HoldStopSource scales the source to 0 for the final copy and starts it
	// again after (clone_consistency stop-source).
	HoldStopSource CloneHold = "stop-source"
)

// CloneFacts is what a clone's tier is chosen from.
type CloneFacts struct {
	Method      fleet.KubeCloneMethod
	Consistency fleet.KubeCloneConsistency
	// SourceRunning: the source instance runs (its replicas are 1).
	SourceRunning bool
	// Driver is the source disk's CSI driver; "" for a disk no CSI driver
	// made (an in-tree or external provisioner: neither snapshots nor
	// clones are fleet's to take).
	Driver string
	// SnapshotClass is the VolumeSnapshotClass serving Driver ("" none),
	// SnapshotClassSet the connection's snapshot_class setting, if any.
	SnapshotClass    string
	SnapshotClassSet string
	// CSIClone: the driver clones volumes (the class's csi_clone).
	CSIClone bool
}

// ClonePlan is how a clone goes.
type ClonePlan struct {
	Tier          CloneTier
	SnapshotClass string
	Hold          CloneHold
	// Why says why, for the job's log.
	Why string
}

// selectCloneTier is the clone's plan, or an error naming the setting the
// cluster cannot honour.
func selectCloneTier(f CloneFacts) (ClonePlan, error) {
	hold := HoldNone
	if f.SourceRunning {
		hold = HoldFreeze
		if f.Consistency == fleet.KubeCloneStopSource {
			hold = HoldStopSource
		}
	}
	misconfigured := func(format string, args ...any) (ClonePlan, error) {
		return ClonePlan{}, kubeerr.New(kubeerr.Misconfigured, format, args...)
	}
	snapshot := func(why string) ClonePlan {
		return ClonePlan{Tier: TierSnapshot, SnapshotClass: f.SnapshotClass, Hold: hold, Why: why}
	}
	switch f.Method {
	case fleet.KubeCloneSnapshot:
		switch {
		case f.Driver == "":
			return misconfigured("clone_method is snapshot, but the instance's disk is not a CSI volume: set clone_method to auto or copy")
		case f.SnapshotClass == "" && f.SnapshotClassSet != "":
			return misconfigured("clone_method is snapshot, but snapshot class %q does not exist or serves another driver than %s", f.SnapshotClassSet, f.Driver)
		case f.SnapshotClass == "":
			return misconfigured("clone_method is snapshot, but no VolumeSnapshotClass serves %s: install one, or set clone_method to auto or copy", f.Driver)
		}
		return snapshot("clone_method snapshot"), nil
	case fleet.KubeCloneCSIClone:
		switch {
		case f.Driver == "":
			return misconfigured("clone_method is csi-clone, but the instance's disk is not a CSI volume: set clone_method to auto or copy")
		case f.SourceRunning && hold != HoldStopSource:
			return misconfigured("clone_method is csi-clone, which clones a stopped instance's disk: stop the instance first, or set clone_consistency to stop-source")
		}
		return ClonePlan{Tier: TierCSIClone, Hold: hold, Why: "clone_method csi-clone"}, nil
	case fleet.KubeCloneCopy:
		return ClonePlan{Tier: TierCopy, Hold: hold, Why: "clone_method copy"}, nil
	}
	switch {
	case f.Driver != "" && f.SnapshotClass != "":
		return snapshot("snapshot class " + f.SnapshotClass + " serves " + f.Driver), nil
	case f.Driver != "" && !f.SourceRunning && f.CSIClone:
		return ClonePlan{Tier: TierCSIClone, Hold: HoldNone, Why: "the instance is stopped and " + f.Driver + " clones volumes"}, nil
	case f.Driver == "":
		return ClonePlan{Tier: TierCopy, Hold: hold, Why: "the instance's disk is not a CSI volume"}, nil
	}
	return ClonePlan{Tier: TierCopy, Hold: hold, Why: "no VolumeSnapshotClass serves " + f.Driver}, nil
}
