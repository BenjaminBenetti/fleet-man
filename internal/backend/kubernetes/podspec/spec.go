package podspec

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// PodSpec is an instance pod as Translate decided it: everything the
// captured docker run, its image and the cluster connection say about the
// dev container and its pod, in terms a renderer maps onto Kubernetes
// objects one field at a time. It holds no environment value (those are in
// Secrets.Env, referenced by Env) and no other secret, so it can be logged,
// hashed and kept in a golden file.
//
// Fields the renderer adds on its own (fleet's persisted-root mounts, the
// spec Secret mount, Harden's defaults, labels, the delivered image
// reference) are not here.
type PodSpec struct {
	// Image is the image the run names, as built or pulled on the daemon host.
	Image Image `json:"image"`
	// PID1 is what pod-init runs as the container's main process
	// (pid1.json in the spec Secret).
	PID1 PID1 `json:"pid1"`
	// Env is the container environment the run sets, in order: each entry's
	// value is the Secrets.Env entry named Key. The image's own environment
	// is applied by the container runtime, as on docker.
	Env []EnvRef `json:"env,omitempty"`
	// TTY and Stdin give the container a terminal and an open stdin
	// (docker run -t, -i).
	TTY   bool `json:"tty,omitempty"`
	Stdin bool `json:"stdin,omitempty"`

	Security  Security  `json:"security"`
	Resources Resources `json:"resources"`
	// DiskSize is the instance disk to request: the connection's disk_size,
	// raised to hostRequirements.storage.
	DiskSize string `json:"diskSize"`

	// Mounts are the run's mounts (and the image's volumes, and fleet's own
	// mounts), each classified by where its data lives.
	Mounts []Mount `json:"mounts,omitempty"`
	// ReadOnlyRoot is --read-only: the persisted root's mounts are mounted
	// read-only (volumes and tmpfs stay writable).
	ReadOnlyRoot bool `json:"readOnlyRoot,omitempty"`

	// Ports are the run's published ports (-p, -P), served by listeners on
	// the daemon host.
	Ports []Port `json:"ports,omitempty"`
	// HostNetwork, HostIPC and HostPID put the pod in the node's namespaces
	// (--network host, --ipc host, --pid host).
	HostNetwork bool `json:"hostNetwork,omitempty"`
	HostIPC     bool `json:"hostIPC,omitempty"`
	HostPID     bool `json:"hostPID,omitempty"`
	// HostAliases are /etc/hosts entries (--add-host, a --hostname the pod
	// cannot take).
	HostAliases []HostAlias `json:"hostAliases,omitempty"`
	// HostGateway mirrors the daemon host's listening ports on HostGatewayIP
	// inside the pod (--add-host <name>:host-gateway), but for GatewaySkip:
	// the ports the devcontainer serves in the pod itself (its published
	// container ports, its forwardPorts), which the mirror would hold
	// against the pod's own listeners.
	HostGateway bool     `json:"hostGateway,omitempty"`
	GatewaySkip []uint16 `json:"gatewaySkip,omitempty"`
	// HostSockets are the daemon-host sockets the run binds (other than the
	// ssh-agent's, which PID1.Links point at fleet's agent relay), one per
	// socket of a bound socket directory: the pod's agent binds each target,
	// and fleetd relays its connections to the recorded source.
	HostSockets []hostrelay.Socket `json:"hostSockets,omitempty"`
	// DNS adds to the pod's resolver configuration (--dns, --dns-search,
	// --dns-option).
	DNS *DNSConfig `json:"dns,omitempty"`
	// SupplementalGroups are --group-add's groups.
	SupplementalGroups []int64 `json:"supplementalGroups,omitempty"`
	// Sysctls are --sysctl's, every one of them in Kubernetes' safe set.
	Sysctls []Sysctl `json:"sysctls,omitempty"`
	// RuntimeClassName is the RuntimeClass the pod runs under: the
	// connection's runtime_class, --runtime, or the sysbox class that serves
	// --privileged.
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// NodeSelector pins the pod to nodes of the image's platform, and holds
	// the connection's node_selector.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// TerminationGracePeriodSeconds is --stop-timeout; nil keeps the
	// renderer's default.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
	// Annotations the pod needs for what was translated (the sysbox user
	// namespace mode). They never hold a run's values.
	Annotations map[string]string `json:"annotations,omitempty"`
	// DooD is the docker engine of a run that reaches one (a docker.sock
	// bind, --use-api-socket): the pod's own, a sidecar, which needs
	// privileged pods allowed or a sysbox runtime class. nil without one.
	DooD *DooD `json:"dood,omitempty"`

	// DebugGrants are the debug-only grants the run asked for (SYS_PTRACE,
	// unconfined seccomp or AppArmor), as written ("--cap-add SYS_PTRACE").
	// They are in Security unless DebugDropped.
	DebugGrants []string `json:"debugGrants,omitempty"`
	// DebugDropped marks a render without the debug grants, after admission
	// denied them (Input.DropDebugGrants).
	DebugDropped bool `json:"debugDropped,omitempty"`

	// Warnings are the visible differences from docker this pod has, one
	// line each, for the instance's provisioning log and warn banner. None
	// carries an environment value.
	Warnings []string `json:"warnings,omitempty"`
	// Options is every option the run gave, with its outcome in the table.
	Options []OptionOutcome `json:"options"`
}

// DooD is the docker-outside-of-docker sidecar (DockerSidecar): a docker
// engine in the pod whose socket the dev container sees at each
// MountDockerSocket target, its data on the instance disk
// (vol/fleet-docker), its config shared with the dev container
// (DockerConfig), and its networks at the pod network's MTU. The containers
// it runs are pod-local.
type DooD struct {
	// Privileged runs the sidecar privileged; false under a sysbox runtime
	// class, which gives the engine what it needs without privilege on the
	// node.
	Privileged bool `json:"privileged,omitempty"`
	// AddressPools are the engine's default-address-pools (the connection's
	// dind_address_pool); none keeps docker's own.
	AddressPools []fleet.DockerAddressPool `json:"addressPools,omitempty"`
}

// Image is the image a run names.
type Image struct {
	// Ref is the reference the run names, on the daemon host (a built image's
	// local tag, or a registry reference).
	Ref string `json:"ref"`
	// ID is the image's id on the daemon host.
	ID string `json:"id,omitempty"`
	// Os, Arch and Variant are the image's platform.
	Os      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Variant string `json:"variant,omitempty"`
}

// EnvRef is one container environment variable whose value is in the
// environment Secret.
type EnvRef struct {
	Name string `json:"name"`
	// Key is the variable's key in Secrets.Env.
	Key string `json:"key"`
}

// Security is the dev container's security context.
type Security struct {
	Privileged bool `json:"privileged,omitempty"`
	// CapAdd and CapDrop are capability names without the CAP_ prefix
	// ("SYS_PTRACE"), or ALL.
	CapAdd  []string `json:"capAdd,omitempty"`
	CapDrop []string `json:"capDrop,omitempty"`
	// Seccomp is the seccomp profile type: SeccompUnconfined, or "" for the
	// renderer's default (RuntimeDefault).
	Seccomp string `json:"seccomp,omitempty"`
	// AppArmor is the AppArmor profile: AppArmorUnconfined,
	// AppArmorRuntimeDefault, "localhost/<profile>" for a profile loaded on
	// the nodes, or "" for the default.
	AppArmor string `json:"appArmor,omitempty"`
	// SELinux holds --security-opt label= options; Level is never set here
	// (Harden gives every pod of a share-group one level).
	SELinux *SELinux `json:"seLinux,omitempty"`
	// NoNewPrivileges is --security-opt no-new-privileges
	// (allowPrivilegeEscalation false).
	NoNewPrivileges bool `json:"noNewPrivileges,omitempty"`
	// ReadOnlyRootFilesystem is --read-only.
	ReadOnlyRootFilesystem bool `json:"readOnlyRootFilesystem,omitempty"`
}

// The seccomp and AppArmor profiles Security names.
const (
	SeccompUnconfined      = "Unconfined"
	AppArmorUnconfined     = "Unconfined"
	AppArmorRuntimeDefault = "RuntimeDefault"
	// AppArmorLocalhostPrefix starts a profile loaded on the nodes.
	AppArmorLocalhostPrefix = "localhost/"
)

// SELinux is the dev container's SELinux options.
type SELinux struct {
	User string `json:"user,omitempty"`
	Role string `json:"role,omitempty"`
	Type string `json:"type,omitempty"`
}

// Resources are the dev container's resource requests and limits, as
// Kubernetes quantities keyed by resource name ("cpu", "memory", a GPU
// resource).
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// MountKind says where a mount's data lives on a cluster.
type MountKind string

const (
	// MountWorkspace is the host workspace (or a folder in it), seeded onto
	// the instance disk's ws/ and mounted from there: SubPath is the folder
	// relative to the workspace root ("" for the root).
	MountWorkspace MountKind = "workspace"
	// MountVolume is a volume that belongs to this instance (a
	// ${devcontainerId} volume, an anonymous one, an image VOLUME), on the
	// instance disk's vol/: SubPath is its folder there.
	MountVolume MountKind = "volume"
	// MountSharedVolume is any other named volume, which on docker every
	// container naming it shares: it lives on the fleet's share, SubPath
	// being its folder there.
	MountSharedVolume MountKind = "shared-volume"
	// MountTmpfs is a memory-backed directory (--tmpfs, --shm-size).
	MountTmpfs MountKind = "tmpfs"
	// MountSSHAgent is a bind of the user's ssh-agent socket: the target
	// becomes a link to fleet's agent relay in the control directory
	// (agentsock.ContainerSocketPath).
	MountSSHAgent MountKind = "ssh-agent"
	// MountHostSocket is a bind of another unix socket on the daemon host
	// (gpg, X11, Wayland, pulse): an agent-bound socket at the target relays
	// each connection to Source.
	MountHostSocket MountKind = "host-socket"
	// MountHostSocketDir is a bind of a daemon-host directory holding only
	// sockets (/tmp/.X11-unix): each one is relayed as for MountHostSocket.
	MountHostSocketDir MountKind = "host-socket-dir"
	// MountHostDir and MountHostFile are binds of a daemon-host directory or
	// file (~/.aws, ~/.gitconfig): its content is kept in step with Source by
	// the fleet's sync (both ways, or host to pod for a read-only bind).
	MountHostDir  MountKind = "host-dir"
	MountHostFile MountKind = "host-file"
	// MountDockerSocket is a bind of the docker engine's socket: the target
	// becomes a link to the socket of the pod's own engine, the DooD sidecar
	// (DockerSocket).
	MountDockerSocket MountKind = "docker-socket"
	// MountDevice is --device: the node's device node, in a privileged pod.
	MountDevice MountKind = "device"
	// MountFleet is one of fleet's own mounts (agent state, shared files,
	// custom mounts): SubPath is its folder on the fleet's share.
	MountFleet MountKind = "fleet"
)

// Mount is one mount of the dev container.
type Mount struct {
	Kind MountKind `json:"kind"`
	// Target is the absolute path in the container.
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	// Source is what the run named: a daemon-host path for a bind or a
	// device, a volume's name.
	Source string `json:"source,omitempty"`
	// SubPath is the mount's folder in its storage (see each kind).
	SubPath string `json:"subPath,omitempty"`
	// CopyUp fills a new volume from the image's content at Target, as
	// docker does (not with volume-nocopy).
	CopyUp bool `json:"copyUp,omitempty"`
	// Anonymous marks a volume no other container can name (an anonymous or
	// image VOLUME): docker gives a rebuilt container a fresh one.
	Anonymous bool `json:"anonymous,omitempty"`
	// SizeLimit caps a tmpfs, as a Kubernetes quantity.
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// Port is one published port: a listener on the daemon host at HostIP and
// HostPort (0 picks one) forwarding to ContainerPort in the pod.
type Port struct {
	HostIP        string `json:"hostIP"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	// Protocol is "TCP"; the cluster forwards nothing else.
	Protocol string `json:"protocol"`
}

// HostAlias is a set of hostnames for one address in the pod's /etc/hosts.
type HostAlias struct {
	IP        string   `json:"ip"`
	Hostnames []string `json:"hostnames"`
}

// HostGatewayIP is where a pod reaches the daemon host's mirrored ports when
// its devcontainer declares host-gateway: a loopback address the pod agent
// listens on.
const HostGatewayIP = podwire.HostGatewayAddr

// DNSConfig adds to the pod's resolver configuration.
type DNSConfig struct {
	Nameservers []string    `json:"nameservers,omitempty"`
	Searches    []string    `json:"searches,omitempty"`
	Options     []DNSOption `json:"options,omitempty"`
}

// DNSOption is one resolver option ("ndots:2" is Name ndots, Value 2).
type DNSOption struct {
	Name  string  `json:"name"`
	Value *string `json:"value,omitempty"`
}

// Sysctl is one namespaced kernel parameter.
type Sysctl struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OptionOutcome is how the table treated one option a run gave.
type OptionOutcome struct {
	// Option is the canonical name ("env", "network").
	Option  string  `json:"option"`
	Outcome Outcome `json:"outcome"`
}
