package fleet

import "time"

// KubeCluster is one daemon-owned connection to a namespace on a Kubernetes
// cluster, with every per-cluster setting (a different namespace is another
// connection). The daemon keeps the records in ~/.fleet/kube_clusters.json on
// its host; fleetgrpc.KubeCluster mirrors it field for field (protoconv
// converts), except GatewayCredential, which never leaves the daemon.
//
// Every setting is "unset = default": an empty string (the string enums
// included) or a nil pointer takes the default named on the field, so a record
// written before a setting existed keeps working. Sizes are Kubernetes
// quantities ("50Gi") and durations Go durations ("30s"); the daemon validates
// them.
type KubeCluster struct {
	// --- identity ---

	// ID names the connection for good: object labels, instance records
	// (Instance.KubeCluster), ContainerIDs and deploy targets carry it. The
	// daemon assigns it when the connection is added and never changes it.
	ID string `json:"id"`
	// DisplayName is the user-facing name, unique among the daemon's
	// connections. Empty shows the ID.
	DisplayName string `json:"display_name,omitempty"`
	// Context is the kubeconfig context the connection uses.
	Context string `json:"context"`
	// KubeconfigPaths are the kubeconfig files on the daemon host the context
	// is read from, pinned when the connection is added.
	KubeconfigPaths []string `json:"kubeconfig_paths,omitempty"`
	// Namespace every object of this connection lives in.
	Namespace string `json:"namespace"`
	// Server is the API server URL the context pointed at when last checked.
	Server string `json:"server,omitempty"`
	// ClusterUID is the kube-system namespace UID, recorded when the
	// connection is added; re-pointing a connection in use to another context
	// or kubeconfig needs the new target to report the same one.
	ClusterUID string `json:"cluster_uid,omitempty"`
	// ExtraPath is prepended to PATH for the kubeconfig's exec credential
	// plugins on the daemon host.
	ExtraPath []string `json:"extra_path,omitempty"`
	// PluginEnv is environment for the exec credential plugins, one variable
	// per entry: "NAME" passes the daemon's own value through, "NAME=value"
	// sets it. Names are allowlisted, and a value is refused for a name that
	// looks secret (*SECRET*, *TOKEN*, *PASSWORD*, *KEY*), so the record,
	// which every client can read, never holds a credential. The daemon
	// validates entries.
	PluginEnv []string `json:"plugin_env,omitempty"`

	// --- instance storage ---

	// StorageClass for instance root disks; empty = auto.
	StorageClass string `json:"storage_class,omitempty"`
	// DiskSize of each instance root disk; empty = DefaultKubeDiskSize.
	DiskSize string `json:"disk_size,omitempty"`
	// AutoExpandDisk grows a nearly full disk where the class allows it; nil
	// = on (see AutoExpandsDisk).
	AutoExpandDisk *bool `json:"auto_expand_disk,omitempty"`
	// SnapshotClass for clone snapshots; empty = auto.
	SnapshotClass string `json:"snapshot_class,omitempty"`
	// CloneMethod and CloneConsistency choose how Clone copies an instance.
	CloneMethod      KubeCloneMethod      `json:"clone_method,omitempty"`
	CloneConsistency KubeCloneConsistency `json:"clone_consistency,omitempty"`
	// BackupWorkspaceOnStop exports the workspace to the daemon host on every
	// stop (for node-local storage).
	BackupWorkspaceOnStop bool `json:"backup_workspace_on_stop,omitempty"`
	// NodeLocalAck acknowledges that node-local storage on a multi-node
	// cluster loses an instance's data with its node.
	NodeLocalAck bool `json:"node_local_ack,omitempty"`

	// --- fleet share ---

	// SharedStorageClass for each fleet's share; empty = auto.
	SharedStorageClass string `json:"shared_storage_class,omitempty"`
	// ShareSize of each fleet's share; empty = DefaultKubeShareSize.
	ShareSize      string             `json:"share_size,omitempty"`
	ShareRetention KubeShareRetention `json:"share_retention,omitempty"`
	// Agent logins are per location (the daemon host, and each cluster);
	// a record saved with the removed credential_scope key loads as that.

	// --- caches ---

	// CacheStorageClass for the fleet cache volumes; empty = auto.
	CacheStorageClass string            `json:"cache_storage_class,omitempty"`
	CacheSizes        KubeCacheSizes    `json:"cache_sizes"`
	ServiceImages     KubeServiceImages `json:"service_images"`
	// AllowSharedBuildkit permits a fleet's shared buildkit server here (the
	// fleet's own toggle decides); nil = allowed.
	AllowSharedBuildkit *bool `json:"allow_shared_buildkit,omitempty"`
	// DeleteCacheVolumesOnDestroy deletes a fleet's cache volumes with the
	// fleet instead of keeping them for a warm restart.
	DeleteCacheVolumesOnDestroy bool `json:"delete_cache_volumes_on_destroy,omitempty"`
	// DinDAddressPool for docker-in-docker networks; empty = docker's.
	DinDAddressPool string `json:"dind_address_pool,omitempty"`
	// BlockMetadata denies pod egress to the cloud metadata range; nil = on.
	BlockMetadata *bool `json:"block_metadata,omitempty"`

	// --- pod shape and security ---

	Resources KubeResources `json:"resources"`
	// MemoryLimit set explicitly when a LimitRange would inject a lower one;
	// empty = DefaultKubeMemoryLimit.
	MemoryLimit  string            `json:"memory_limit,omitempty"`
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	Tolerations  []KubeToleration  `json:"tolerations,omitempty"`
	// AllowPrivileged permits privileged pods (the node becomes the
	// isolation boundary); nil = auto: on for a detected local cluster
	// (minikube, kind, k3d, Docker Desktop), off elsewhere.
	AllowPrivileged   *bool                 `json:"allow_privileged,omitempty"`
	DebugCapabilities KubeDebugCapabilities `json:"debug_capabilities,omitempty"`
	// RuntimeClass for instance pods (e.g. sysbox); empty = the cluster's.
	RuntimeClass string `json:"runtime_class,omitempty"`
	// ServiceAccount instance pods run as; empty = the namespace default.
	ServiceAccount string `json:"service_account,omitempty"`
	// PodOverridesYAML is merged into every instance pod spec (allowlisted
	// fields; host namespaces and hostPath only with AllowPrivileged).
	PodOverridesYAML string `json:"pod_overrides_yaml,omitempty"`
	// ProtectFromDrain adds a PodDisruptionBudget to each instance.
	ProtectFromDrain bool            `json:"protect_from_drain,omitempty"`
	HostGateway      KubeHostGateway `json:"host_gateway,omitempty"`

	// --- quotas ---

	// MaxInstances on this connection; 0 = no limit.
	MaxInstances int `json:"max_instances,omitempty"`
	// MaxTotalDisk across this connection's instance disks; empty = no limit.
	MaxTotalDisk string `json:"max_total_disk,omitempty"`
	// AutomationFireInterval is the shortest time between two firings of one
	// trigger whose agent runs here; empty = DefaultKubeAutomationFireInterval,
	// "0" = no limit.
	AutomationFireInterval string `json:"automation_fire_interval,omitempty"`
	// AutomationMaxPerAgent caps one agent's concurrent runs here; nil =
	// DefaultKubeAutomationMaxPerAgent, 0 = no limit.
	AutomationMaxPerAgent *int `json:"automation_max_per_agent,omitempty"`

	// --- image ---

	Image KubeImageSettings `json:"image"`

	// --- workspace ---

	// RebuildExport is how Rebuild brings the workspace back to the daemon
	// host before it rebuilds: "" or "auto" exports it from the instance,
	// "off" builds from the daemon host's checkout as it is.
	RebuildExport string `json:"rebuild_export,omitempty"`

	// --- bookkeeping (daemon-owned) ---

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// VerifiedAt is when the last full preflight passed.
	VerifiedAt time.Time `json:"verified_at"`
	// Revision increases on every change to the record.
	Revision int64 `json:"revision"`
}

// KubeResources is what each instance pod requests.
type KubeResources struct {
	// CPURequest; empty = DefaultKubeCPURequest.
	CPURequest string `json:"cpu_request,omitempty"`
	// MemoryRequest; empty = DefaultKubeMemoryRequest.
	MemoryRequest string `json:"memory_request,omitempty"`
	// AutopilotMemory is the memory request on GKE Autopilot, where limits
	// equal requests; empty = DefaultKubeAutopilotMemory.
	AutopilotMemory string `json:"autopilot_memory,omitempty"`
}

// KubeToleration mirrors a core/v1 Toleration.
type KubeToleration struct {
	Key string `json:"key,omitempty"`
	// Operator: "Exists" or "Equal" (empty = Equal).
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	// Effect: "NoSchedule", "PreferNoSchedule", "NoExecute" (empty = all).
	Effect            string `json:"effect,omitempty"`
	TolerationSeconds *int64 `json:"toleration_seconds,omitempty"`
}

// KubeCacheSizes sizes each fleet cache volume, one field per SharedService.
// Empty takes the default (DefaultKubeCacheSizes).
type KubeCacheSizes struct {
	Buildkit   string `json:"buildkit,omitempty"`
	DebCache   string `json:"debcache,omitempty"`
	ImageCache string `json:"imagecache,omitempty"`
}

// KubeServiceImages overrides the images of the services fleet runs in the
// namespace. Empty takes the digest-pinned default.
type KubeServiceImages struct {
	// Buildkit is the rootful image, used only where privileged pods are
	// allowed; BuildkitRootless is the default.
	Buildkit         string `json:"buildkit,omitempty"`
	BuildkitRootless string `json:"buildkit_rootless,omitempty"`
	DebCache         string `json:"debcache,omitempty"`
	ImageCache       string `json:"imagecache,omitempty"`
	// DinD is the docker-in-docker sidecar for docker-outside-of-docker
	// devcontainers.
	DinD string `json:"dind,omitempty"`
}

// KubeImageSettings is how built devcontainer images reach the cluster.
type KubeImageSettings struct {
	Mode KubeImageMode `json:"mode,omitempty"`
	// Registry images are pushed to ("registry.example.com/team").
	Registry string `json:"registry,omitempty"`
	// InsecureRegistry allows plain HTTP or an untrusted certificate.
	InsecureRegistry bool `json:"insecure_registry,omitempty"`
	// PullSecrets are existing image pull Secrets in the namespace.
	PullSecrets []string `json:"pull_secrets,omitempty"`
	// ManagedPullSecret builds a pull Secret from the registry's own scoped
	// credential on the daemon host; nil = on.
	ManagedPullSecret *bool `json:"managed_pull_secret,omitempty"`
	// GatewayCredential is the pull credential the gateway delivery serves
	// this cluster's images with. The daemon generates and rotates it; it is
	// never returned to a client and never set by one, so it has no proto
	// field and protoconv leaves it out in both directions.
	GatewayCredential string `json:"gateway_credential,omitempty"`
	// Builder is the docker buildx builder images are built with; empty = the
	// default builder.
	Builder string `json:"builder,omitempty"`
	// Platform to build for ("linux/arm64"); empty = the nodes' platform.
	Platform string `json:"platform,omitempty"`
	// Retention of pushed images nothing uses any more; empty =
	// DefaultKubeImageRetention.
	Retention string `json:"retention,omitempty"`
	// UploadPacePercent caps registry uploads through the gateway at this
	// share of the measured uplink; nil = DefaultKubeUploadPacePercent, 0 =
	// unpaced.
	UploadPacePercent *int `json:"upload_pace_percent,omitempty"`
}

// The defaults an unset KubeCluster setting takes.
const (
	DefaultKubeDiskSize               = "50Gi"
	DefaultKubeShareSize              = "10Gi"
	DefaultKubeMemoryLimit            = "8Gi"
	DefaultKubeCPURequest             = "500m"
	DefaultKubeMemoryRequest          = "2Gi"
	DefaultKubeAutopilotMemory        = "4Gi"
	DefaultKubeAutomationFireInterval = 30 * time.Second
	DefaultKubeAutomationMaxPerAgent  = 4
	DefaultKubeImageRetention         = 72 * time.Hour
	DefaultKubeUploadPacePercent      = 80
)

// DefaultKubeCacheSizes are the cache volume sizes an unset KubeCacheSizes
// field takes.
var DefaultKubeCacheSizes = KubeCacheSizes{Buildkit: "20Gi", DebCache: "10Gi", ImageCache: "20Gi"}

// GetDisplayName returns the connection's user-facing name, falling back to
// its ID.
func (k *KubeCluster) GetDisplayName() string {
	if k.DisplayName == "" {
		return k.ID
	}
	return k.DisplayName
}

// AutomationInterval is the shortest time between two firings of one
// trigger whose agent runs here: DefaultKubeAutomationFireInterval when
// unset (or unparseable, which validation refuses), 0 when turned off.
func (k *KubeCluster) AutomationInterval() time.Duration {
	if k.AutomationFireInterval == "" {
		return DefaultKubeAutomationFireInterval
	}
	if k.AutomationFireInterval == "0" {
		return 0
	}
	d, err := time.ParseDuration(k.AutomationFireInterval)
	if err != nil || d < 0 {
		return DefaultKubeAutomationFireInterval
	}
	return d
}

// AutomationRunLimit is how many runs one agent may have here at once:
// DefaultKubeAutomationMaxPerAgent when unset, 0 for no limit.
func (k *KubeCluster) AutomationRunLimit() int {
	if k.AutomationMaxPerAgent == nil {
		return DefaultKubeAutomationMaxPerAgent
	}
	return max(*k.AutomationMaxPerAgent, 0)
}

// AutoExpandsDisk reports whether disks grow when nearly full (default on).
func (k *KubeCluster) AutoExpandsDisk() bool { return boolOr(k.AutoExpandDisk, true) }

// AllowsSharedBuildkit reports whether a fleet's shared buildkit server may
// run here (default allowed).
func (k *KubeCluster) AllowsSharedBuildkit() bool { return boolOr(k.AllowSharedBuildkit, true) }

// BlocksMetadata reports whether pod egress to the cloud metadata range is
// denied (default on).
func (k *KubeCluster) BlocksMetadata() bool { return boolOr(k.BlockMetadata, true) }

// ManagesPullSecret reports whether a pull Secret is built from the
// registry's credential (default on).
func (k *KubeImageSettings) ManagesPullSecret() bool { return boolOr(k.ManagedPullSecret, true) }

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
