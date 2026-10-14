package fleet

// The string-valued enums of KubeCluster settings. Each one's empty value
// means its "auto" default, so a record that never set it reads as auto; they
// travel as plain strings on the wire (fleetgrpc.KubeCluster), like
// TriggerType does.

// KubeCloneMethod chooses how Clone copies an instance's disk.
type KubeCloneMethod string

const (
	// KubeCloneAuto (also the empty value) picks the best the cluster offers.
	KubeCloneAuto KubeCloneMethod = "auto"
	// KubeCloneSnapshot restores the copy from a VolumeSnapshot.
	KubeCloneSnapshot KubeCloneMethod = "snapshot"
	// KubeCloneCSIClone clones the volume through the CSI driver.
	KubeCloneCSIClone KubeCloneMethod = "csi-clone"
	// KubeCloneCopy copies the files with a copier Job.
	KubeCloneCopy KubeCloneMethod = "copy"
)

// Valid reports whether m is empty or a known clone method.
func (m KubeCloneMethod) Valid() bool {
	switch m {
	case "", KubeCloneAuto, KubeCloneSnapshot, KubeCloneCSIClone, KubeCloneCopy:
		return true
	}
	return false
}

// KubeCloneConsistency chooses whether Clone may copy a live source.
type KubeCloneConsistency string

const (
	// KubeCloneConsistencyAuto (also the empty value) copies the source live.
	KubeCloneConsistencyAuto KubeCloneConsistency = "auto"
	// KubeCloneStopSource stops the source for the final copy.
	KubeCloneStopSource KubeCloneConsistency = "stop-source"
)

// Valid reports whether c is empty or a known clone consistency.
func (c KubeCloneConsistency) Valid() bool {
	switch c {
	case "", KubeCloneConsistencyAuto, KubeCloneStopSource:
		return true
	}
	return false
}

// KubeShareRetention chooses what fleet destroy does with the fleet's share.
type KubeShareRetention string

const (
	// KubeShareDelete (also the empty value) deletes it after a flush.
	KubeShareDelete KubeShareRetention = "delete"
	// KubeShareRetain keeps it.
	KubeShareRetain KubeShareRetention = "retain"
)

// Valid reports whether r is empty or a known share retention.
func (r KubeShareRetention) Valid() bool {
	switch r {
	case "", KubeShareDelete, KubeShareRetain:
		return true
	}
	return false
}

// KubeDebugCapabilities chooses what happens to the debug grants a
// devcontainer asks for (SYS_PTRACE, unconfined seccomp).
type KubeDebugCapabilities string

const (
	// KubeDebugCapabilitiesAuto (also the empty value) asks for them and drops
	// them with a warning when admission denies them.
	KubeDebugCapabilitiesAuto KubeDebugCapabilities = "auto"
	// KubeDebugCapabilitiesRequire fails the create instead.
	KubeDebugCapabilitiesRequire KubeDebugCapabilities = "require"
)

// Valid reports whether d is empty or a known debug capabilities policy.
func (d KubeDebugCapabilities) Valid() bool {
	switch d {
	case "", KubeDebugCapabilitiesAuto, KubeDebugCapabilitiesRequire:
		return true
	}
	return false
}

// KubeHostGateway chooses whether pods reach the daemon host's ports as
// host.docker.internal.
type KubeHostGateway string

const (
	// KubeHostGatewayAuto (also the empty value) mirrors them when the
	// devcontainer declares host-gateway.
	KubeHostGatewayAuto KubeHostGateway = "auto"
	// KubeHostGatewayOff never does.
	KubeHostGatewayOff KubeHostGateway = "off"
)

// Valid reports whether g is empty or a known host gateway mode.
func (g KubeHostGateway) Valid() bool {
	switch g {
	case "", KubeHostGatewayAuto, KubeHostGatewayOff:
		return true
	}
	return false
}

// KubeImageMode chooses how built images reach the cluster.
type KubeImageMode string

const (
	// KubeImageAuto (also the empty value): node load on a local cluster,
	// else the registry when one is set, else the gateway.
	KubeImageAuto     KubeImageMode = "auto"
	KubeImageNodeLoad KubeImageMode = "node-load"
	KubeImageRegistry KubeImageMode = "registry"
	KubeImageGateway  KubeImageMode = "gateway"
)

// Valid reports whether m is empty or a known image delivery mode.
func (m KubeImageMode) Valid() bool {
	switch m {
	case "", KubeImageAuto, KubeImageNodeLoad, KubeImageRegistry, KubeImageGateway:
		return true
	}
	return false
}
