package podspec

import (
	"path"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The instance disk's layout (root/<d>, root-extra/<d>, ws, vol/<name>,
// fleet, meta) and where the instance pod sees fleet's own folders
// (/opt/fleet, its spec and run dirs) are the podseed package's: the seed
// writes the disk, the renderers mount it.

// Command is the dev container's command: pod-init, which runs the
// captured start script as PID 1 (pid1.json in the spec Secret).
func Command() []string {
	return []string{podseed.CurrentBinary, "pod-init", "--spec", path.Join(podseed.SpecDir, PID1File)}
}

// JobDiskPath is where a Job pod mounts the whole instance disk unless its
// JobInput says otherwise: the job spec's PVC path (podseed.SeedSpec.PVC).
const JobDiskPath = "/fleet-pvc"
