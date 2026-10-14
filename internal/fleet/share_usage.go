package fleet

// ShareUsage is a fleet's share on one Kubernetes connection: the volume its
// instances there keep the fleet's agent dirs, shared files and custom mounts
// on, for the cluster editor's Storage section and `fleet sync status`.
type ShareUsage struct {
	// Cluster is the connection, Location its display name, Fleet the
	// fleet, Claim the share's PVC.
	Cluster  string
	Location string
	Fleet    string
	Claim    string
	// UsedBytes and CapacityBytes are the share's use and size; UsedBytes is
	// 0 while no running pod of the fleet could measure it (Measured).
	UsedBytes     int64
	CapacityBytes int64
	Measured      bool
	// Expandable: the share grows by half when it is nearly full
	// (ShareFullPercent).
	Expandable bool
	// Warning is the line shown for a nearly full share that cannot grow
	// ("93% full; its StorageClass cannot grow it"); empty when all is well.
	Warning string
}

// ShareFullPercent is how full a share gets before it grows (where its
// StorageClass allows and the connection auto-expands) or warns: as cache
// volumes and instance disks.
const ShareFullPercent = CacheFullPercent

// Percent is how full the share is (0 when unknown).
func (u ShareUsage) Percent() int {
	if u.CapacityBytes <= 0 || !u.Measured {
		return 0
	}
	return int(u.UsedBytes * 100 / u.CapacityBytes)
}
