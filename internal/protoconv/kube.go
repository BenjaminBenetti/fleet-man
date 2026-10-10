package protoconv

import (
	"maps"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// KubeClusterToProto maps a cluster connection to the wire. The gateway image
// credential (Image.GatewayCredential) has no proto field and is never sent:
// it stays on the daemon host. Settings travel as stored, unset ones included,
// so a client sees which settings take their defaults. The result shares no
// slice, map or pointer with k (nor does KubeClusterFromProto's with its
// input), so a record pushed to watchers stays as it was sent when the stored
// one changes.
func KubeClusterToProto(k fleet.KubeCluster) *fleetgrpc.KubeCluster {
	pk := &fleetgrpc.KubeCluster{
		Id:                          k.ID,
		DisplayName:                 k.DisplayName,
		Context:                     k.Context,
		KubeconfigPaths:             slices.Clone(k.KubeconfigPaths),
		Namespace:                   k.Namespace,
		Server:                      k.Server,
		ClusterUid:                  k.ClusterUID,
		ExtraPath:                   slices.Clone(k.ExtraPath),
		PluginEnv:                   slices.Clone(k.PluginEnv),
		StorageClass:                k.StorageClass,
		DiskSize:                    k.DiskSize,
		AutoExpandDisk:              copyBool(k.AutoExpandDisk),
		SnapshotClass:               k.SnapshotClass,
		CloneMethod:                 string(k.CloneMethod),
		CloneConsistency:            string(k.CloneConsistency),
		BackupWorkspaceOnStop:       k.BackupWorkspaceOnStop,
		NodeLocalAck:                k.NodeLocalAck,
		SharedStorageClass:          k.SharedStorageClass,
		ShareSize:                   k.ShareSize,
		ShareRetention:              string(k.ShareRetention),
		CredentialScope:             string(k.CredentialScope),
		CacheStorageClass:           k.CacheStorageClass,
		CacheSizes:                  kubeCacheSizesToProto(k.CacheSizes),
		ServiceImages:               kubeServiceImagesToProto(k.ServiceImages),
		AllowSharedBuildkit:         copyBool(k.AllowSharedBuildkit),
		DeleteCacheVolumesOnDestroy: k.DeleteCacheVolumesOnDestroy,
		DindAddressPool:             k.DinDAddressPool,
		BlockMetadata:               copyBool(k.BlockMetadata),
		Resources:                   kubeResourcesToProto(k.Resources),
		MemoryLimit:                 k.MemoryLimit,
		NodeSelector:                maps.Clone(k.NodeSelector),
		Tolerations:                 kubeTolerationsToProto(k.Tolerations),
		AllowPrivileged:             copyBool(k.AllowPrivileged),
		DebugCapabilities:           string(k.DebugCapabilities),
		RuntimeClass:                k.RuntimeClass,
		ServiceAccount:              k.ServiceAccount,
		PodOverridesYaml:            k.PodOverridesYAML,
		ProtectFromDrain:            k.ProtectFromDrain,
		HostGateway:                 string(k.HostGateway),
		MaxInstances:                int32(k.MaxInstances),
		MaxTotalDisk:                k.MaxTotalDisk,
		AutomationFireInterval:      k.AutomationFireInterval,
		AutomationMaxPerAgent:       intToInt32Ptr(k.AutomationMaxPerAgent),
		Image:                       kubeImageSettingsToProto(k.Image),
		RebuildExport:               k.RebuildExport,
		CreatedAt:                   timeToProto(k.CreatedAt),
		UpdatedAt:                   timeToProto(k.UpdatedAt),
		VerifiedAt:                  timeToProto(k.VerifiedAt),
		Revision:                    k.Revision,
	}
	return pk
}

// KubeClusterFromProto reconstructs a cluster connection (zero for nil). The
// gateway image credential is never read from the wire, so a client can never
// set it; a daemon applying a client's record keeps its own.
func KubeClusterFromProto(pk *fleetgrpc.KubeCluster) fleet.KubeCluster {
	if pk == nil {
		return fleet.KubeCluster{}
	}
	return fleet.KubeCluster{
		ID:                          pk.GetId(),
		DisplayName:                 pk.GetDisplayName(),
		Context:                     pk.GetContext(),
		KubeconfigPaths:             slices.Clone(pk.GetKubeconfigPaths()),
		Namespace:                   pk.GetNamespace(),
		Server:                      pk.GetServer(),
		ClusterUID:                  pk.GetClusterUid(),
		ExtraPath:                   slices.Clone(pk.GetExtraPath()),
		PluginEnv:                   slices.Clone(pk.GetPluginEnv()),
		StorageClass:                pk.GetStorageClass(),
		DiskSize:                    pk.GetDiskSize(),
		AutoExpandDisk:              copyBool(pk.AutoExpandDisk),
		SnapshotClass:               pk.GetSnapshotClass(),
		CloneMethod:                 fleet.KubeCloneMethod(pk.GetCloneMethod()),
		CloneConsistency:            fleet.KubeCloneConsistency(pk.GetCloneConsistency()),
		BackupWorkspaceOnStop:       pk.GetBackupWorkspaceOnStop(),
		NodeLocalAck:                pk.GetNodeLocalAck(),
		SharedStorageClass:          pk.GetSharedStorageClass(),
		ShareSize:                   pk.GetShareSize(),
		ShareRetention:              fleet.KubeShareRetention(pk.GetShareRetention()),
		CredentialScope:             fleet.KubeCredentialScope(pk.GetCredentialScope()),
		CacheStorageClass:           pk.GetCacheStorageClass(),
		CacheSizes:                  kubeCacheSizesFromProto(pk.GetCacheSizes()),
		ServiceImages:               kubeServiceImagesFromProto(pk.GetServiceImages()),
		AllowSharedBuildkit:         copyBool(pk.AllowSharedBuildkit),
		DeleteCacheVolumesOnDestroy: pk.GetDeleteCacheVolumesOnDestroy(),
		DinDAddressPool:             pk.GetDindAddressPool(),
		BlockMetadata:               copyBool(pk.BlockMetadata),
		Resources:                   kubeResourcesFromProto(pk.GetResources()),
		MemoryLimit:                 pk.GetMemoryLimit(),
		NodeSelector:                maps.Clone(pk.GetNodeSelector()),
		Tolerations:                 kubeTolerationsFromProto(pk.GetTolerations()),
		AllowPrivileged:             copyBool(pk.AllowPrivileged),
		DebugCapabilities:           fleet.KubeDebugCapabilities(pk.GetDebugCapabilities()),
		RuntimeClass:                pk.GetRuntimeClass(),
		ServiceAccount:              pk.GetServiceAccount(),
		PodOverridesYAML:            pk.GetPodOverridesYaml(),
		ProtectFromDrain:            pk.GetProtectFromDrain(),
		HostGateway:                 fleet.KubeHostGateway(pk.GetHostGateway()),
		MaxInstances:                int(pk.GetMaxInstances()),
		MaxTotalDisk:                pk.GetMaxTotalDisk(),
		AutomationFireInterval:      pk.GetAutomationFireInterval(),
		AutomationMaxPerAgent:       int32ToIntPtr(pk.AutomationMaxPerAgent),
		Image:                       kubeImageSettingsFromProto(pk.GetImage()),
		RebuildExport:               pk.GetRebuildExport(),
		CreatedAt:                   timeFromProto(pk.GetCreatedAt()),
		UpdatedAt:                   timeFromProto(pk.GetUpdatedAt()),
		VerifiedAt:                  timeFromProto(pk.GetVerifiedAt()),
		Revision:                    pk.GetRevision(),
	}
}

// KubeClustersToProto maps a list of cluster connections (nil for empty).
func KubeClustersToProto(in []fleet.KubeCluster) []*fleetgrpc.KubeCluster {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.KubeCluster, 0, len(in))
	for _, k := range in {
		out = append(out, KubeClusterToProto(k))
	}
	return out
}

// KubeClustersFromProto reconstructs a list of cluster connections (nil for
// empty).
func KubeClustersFromProto(in []*fleetgrpc.KubeCluster) []fleet.KubeCluster {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.KubeCluster, 0, len(in))
	for _, pk := range in {
		out = append(out, KubeClusterFromProto(pk))
	}
	return out
}

func kubeResourcesToProto(r fleet.KubeResources) *fleetgrpc.KubeResources {
	return &fleetgrpc.KubeResources{
		CpuRequest:      r.CPURequest,
		MemoryRequest:   r.MemoryRequest,
		AutopilotMemory: r.AutopilotMemory,
	}
}

func kubeResourcesFromProto(pr *fleetgrpc.KubeResources) fleet.KubeResources {
	return fleet.KubeResources{
		CPURequest:      pr.GetCpuRequest(),
		MemoryRequest:   pr.GetMemoryRequest(),
		AutopilotMemory: pr.GetAutopilotMemory(),
	}
}

func kubeTolerationsToProto(in []fleet.KubeToleration) []*fleetgrpc.KubeToleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.KubeToleration, 0, len(in))
	for _, t := range in {
		pt := &fleetgrpc.KubeToleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect}
		if t.TolerationSeconds != nil {
			seconds := *t.TolerationSeconds
			pt.TolerationSeconds = &seconds
		}
		out = append(out, pt)
	}
	return out
}

func kubeTolerationsFromProto(in []*fleetgrpc.KubeToleration) []fleet.KubeToleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.KubeToleration, 0, len(in))
	for _, pt := range in {
		t := fleet.KubeToleration{Key: pt.GetKey(), Operator: pt.GetOperator(), Value: pt.GetValue(), Effect: pt.GetEffect()}
		if pt.TolerationSeconds != nil {
			seconds := pt.GetTolerationSeconds()
			t.TolerationSeconds = &seconds
		}
		out = append(out, t)
	}
	return out
}

func kubeCacheSizesToProto(c fleet.KubeCacheSizes) *fleetgrpc.KubeCacheSizes {
	return &fleetgrpc.KubeCacheSizes{Buildkit: c.Buildkit, Debcache: c.DebCache, Imagecache: c.ImageCache}
}

func kubeCacheSizesFromProto(pc *fleetgrpc.KubeCacheSizes) fleet.KubeCacheSizes {
	return fleet.KubeCacheSizes{Buildkit: pc.GetBuildkit(), DebCache: pc.GetDebcache(), ImageCache: pc.GetImagecache()}
}

func kubeServiceImagesToProto(s fleet.KubeServiceImages) *fleetgrpc.KubeServiceImages {
	return &fleetgrpc.KubeServiceImages{
		Buildkit:         s.Buildkit,
		BuildkitRootless: s.BuildkitRootless,
		Debcache:         s.DebCache,
		Imagecache:       s.ImageCache,
		Dind:             s.DinD,
	}
}

func kubeServiceImagesFromProto(ps *fleetgrpc.KubeServiceImages) fleet.KubeServiceImages {
	return fleet.KubeServiceImages{
		Buildkit:         ps.GetBuildkit(),
		BuildkitRootless: ps.GetBuildkitRootless(),
		DebCache:         ps.GetDebcache(),
		ImageCache:       ps.GetImagecache(),
		DinD:             ps.GetDind(),
	}
}

// kubeImageSettingsToProto leaves GatewayCredential behind: it never leaves
// the daemon host.
func kubeImageSettingsToProto(i fleet.KubeImageSettings) *fleetgrpc.KubeImageSettings {
	return &fleetgrpc.KubeImageSettings{
		Mode:              string(i.Mode),
		Registry:          i.Registry,
		InsecureRegistry:  i.InsecureRegistry,
		PullSecrets:       slices.Clone(i.PullSecrets),
		ManagedPullSecret: copyBool(i.ManagedPullSecret),
		Builder:           i.Builder,
		Platform:          i.Platform,
		Retention:         i.Retention,
		UploadPacePercent: intToInt32Ptr(i.UploadPacePercent),
	}
}

func kubeImageSettingsFromProto(pi *fleetgrpc.KubeImageSettings) fleet.KubeImageSettings {
	if pi == nil {
		// A record sent without image settings: the tri-states below read
		// fields directly, so a nil message must not reach them.
		return fleet.KubeImageSettings{}
	}
	return fleet.KubeImageSettings{
		Mode:              fleet.KubeImageMode(pi.GetMode()),
		Registry:          pi.GetRegistry(),
		InsecureRegistry:  pi.GetInsecureRegistry(),
		PullSecrets:       slices.Clone(pi.GetPullSecrets()),
		ManagedPullSecret: copyBool(pi.ManagedPullSecret),
		Builder:           pi.GetBuilder(),
		Platform:          pi.GetPlatform(),
		Retention:         pi.GetRetention(),
		UploadPacePercent: int32ToIntPtr(pi.UploadPacePercent),
	}
}

// copyBool copies a tri-state *bool, so neither side aliases the other's.
func copyBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	return boolptr(*b)
}

func intToInt32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}

func int32ToIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// timeToProto maps a zero time to nil, like Instance.created_at.
func timeToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func timeFromProto(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
