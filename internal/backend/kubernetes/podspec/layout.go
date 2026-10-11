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

// The docker-outside-of-docker sidecar (PodSpec.DooD): a docker engine in the
// instance pod, a native sidecar of the dev container.
const (
	// DockerSidecar is its container's name.
	DockerSidecar = "fleet-docker"
	// DockerDir is the folder it shares with the dev container (an
	// emptyDir): the engine's socket and its daemon.json.
	DockerDir = podseed.InstallDir + "/docker"
	// DockerSocket is the engine's socket; each MountDockerSocket target is
	// a link to it (pid1.json links).
	DockerSocket = DockerDir + "/docker.sock"
	// DockerConfig is the engine's daemon.json: the sidecar reloads the
	// engine (SIGHUP) whenever it changes, as the dev container cannot
	// signal another container's processes. Like the folder, it is root's:
	// the dev container writes it as root, as /etc/docker/daemon.json.
	DockerConfig = DockerDir + "/daemon.json"
	// DockerConfigEnv names DockerConfig in the dev container's
	// environment: fleet's image cache step merges its registry mirror
	// there instead of into /etc/docker/daemon.json.
	DockerConfigEnv = "FLEET_DOCKERD_CONFIG"
	// DockerDataVolume is the engine's /var/lib/docker on the instance disk,
	// vol/fleet-docker: its images and containers live as long as the
	// instance, through stops, restarts and rebuilds.
	DockerDataVolume = "fleet-docker"
	// DefaultDockerImage is the sidecar's image when the connection names
	// none (service_images.dind): docker's own docker-in-docker image,
	// pinned by digest. An image that replaces it must be one of docker's
	// docker:<version>-dind images (the sidecar runs their docker-init).
	DefaultDockerImage = "docker:29.9.0-dind@sha256:0b6d18a4a222e71c88be6034926b4f6cde2fc33c2218643d03ccfddc65995470"
)
