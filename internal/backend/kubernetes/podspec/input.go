package podspec

import (
	"encoding/json"
	"io/fs"
	"os"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// Input is everything Translate reads.
type Input struct {
	// Capture is the docker run capture-up recorded, values included.
	Capture *kubeshim.Capture
	// Image is the image the run names (docker image inspect).
	Image kubeshim.ImageConfig
	// Merged is the merged devcontainer configuration (devcli.Config.Merged):
	// remoteUser, remoteEnv, userEnvProbe and hostRequirements come from it.
	Merged json.RawMessage
	// WorkspaceDir is the host checkout the run binds as the workspace.
	WorkspaceDir string
	// WorkspaceFolder is the workspace's path in the container.
	WorkspaceFolder string
	// FleetMounts are fleet's own mounts for the instance. They win over a
	// run mount with the same target, as on docker.
	FleetMounts []backend.Mount

	// Cluster is the connection the instance runs on; nil reads as one with
	// every setting at its default.
	Cluster *fleet.KubeCluster
	// Facts is what the connection's preflight found.
	Facts ClusterFacts

	// Host answers the stat of bind sources on the daemon host; nil is the
	// real filesystem.
	Host HostFS
	// HostAgentSockets are the daemon host's paths to the user's ssh-agent
	// (its SSH_AUTH_SOCK, the socket it was started with): a bind of one is
	// served by fleet's agent relay.
	HostAgentSockets []string
	// SSHAuthSock is the agent socket devcontainer-exec-class processes get
	// as SSH_AUTH_SOCK in the pod (exec.json); "" gives them none.
	SSHAuthSock string
	// FleetDir is fleet's own state on the daemon host (~/.fleet): a bind of
	// a folder or file that is it, holds it or is in it is refused, as the
	// fleet's sync would copy the daemon's own files into pods. "" checks
	// nothing.
	FleetDir string
	// LookupGroup resolves a --group-add group name to its gid in the image;
	// nil (or a name it does not know) refuses names, which a pod spec cannot
	// carry.
	LookupGroup func(name string) (int64, bool)

	// DropDebugGrants renders without the debug grants (the re-render after
	// admission denied them; see Denied).
	DropDebugGrants bool
}

// ClusterFacts is what a connection's preflight found about the cluster and
// namespace (fleetgrpc.KubeCapabilities, in the parts Translate reads).
type ClusterFacts struct {
	// Local is a detected local cluster (minikube, kind, k3d, Docker
	// Desktop).
	Local bool
	// Autopilot is GKE Autopilot, where limits equal requests.
	Autopilot bool
	// LimitRangeMemoryLimit is the default memory limit a LimitRange in the
	// namespace gives a container that sets none, as a quantity; "" when no
	// LimitRange does.
	LimitRangeMemoryLimit string
	// RuntimeClasses are the cluster's RuntimeClass names; nil when they
	// could not be read.
	RuntimeClasses []string
	// Nodes are the cluster's nodes, as far as they could be read.
	Nodes []Node
	// GPUResource is the extended resource the nodes offer GPUs as
	// ("nvidia.com/gpu"); "" when none does.
	GPUResource string
	// ServerVersion is the API server's version ("v1.37.0"); "" when it is
	// not known.
	ServerVersion string
	// DiskFileShare names the instance disk's StorageClass when it is a
	// network file share (NFS, EFS, Azure Files, ...) rather than a block
	// disk; "" for a block class, or one not known.
	DiskFileShare string
}

// Node is one node's architecture and allocatable resources (quantities).
type Node struct {
	Arch              string
	AllocatableCPU    string
	AllocatableMemory string
}

// HostFS is the daemon host's filesystem as bind classification sees it.
type HostFS interface {
	// Stat follows symlinks, as a bind mount does.
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

// osFS is the real filesystem.
type osFS struct{}

func (osFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
