package instanceops

import "github.com/BenjaminBenetti/fleet-man/internal/fleet"

// Result reports the outcome of a lifecycle transition: which instance was
// targeted, its status before and after, and whether the call actually changed
// anything (Changed is false for no-op transitions, e.g. stopping an already
// stopped instance).
type Result struct {
	FleetName      string
	InstanceName   string
	PreviousStatus fleet.InstanceStatus
	Status         fleet.InstanceStatus
	Changed        bool
}
