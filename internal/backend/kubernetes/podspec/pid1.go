package podspec

import "github.com/BenjaminBenetti/fleet-man/internal/podinit"

// PID1Format is the version of the pid1.json layout.
const PID1Format = podinit.SpecVersion

// PID1File is the name of the PID 1 spec in the spec Secret, which the pod
// mounts at /opt/fleet/spec.
const PID1File = "pid1.json"

// PID1 is pid1.json: what pod-init, the dev container's PID 1, does before it
// starts the devcontainer's own main process, as the captured docker run
// asked for it. The type is pod-init's own, so the renderer and the in-pod
// reader cannot drift apart.
type PID1 = podinit.Spec

// Rlimit is one resource limit; -1 is unlimited.
type Rlimit = podinit.Rlimit
