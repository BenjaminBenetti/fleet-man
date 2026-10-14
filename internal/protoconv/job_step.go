package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// JobStepToProto maps a domain job step onto the wire enum. fleet.JobStep
// carries fleetgrpc.JobStep's numbers, so this is a plain conversion; the
// test pins every member to its proto name.
func JobStepToProto(s fleet.JobStep) fleetgrpc.JobStep {
	return fleetgrpc.JobStep(s)
}

// JobStepFromProto maps a wire job step to the domain one. A step this build
// does not know (a newer daemon's) keeps its number, and its Label is "".
func JobStepFromProto(s fleetgrpc.JobStep) fleet.JobStep {
	return fleet.JobStep(s)
}
