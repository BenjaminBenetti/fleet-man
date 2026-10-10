package protoconv

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Every field of a cluster connection survives the wire, except the gateway
// image credential, which never travels (see the credential tests below).
func TestKubeClusterRoundTripAllFields(t *testing.T) {
	in := filled[fleet.KubeCluster](t)
	want := in
	want.Image.GatewayCredential = ""
	requireEqual(t, KubeClusterFromProto(KubeClusterToProto(in)), want)
}

func TestKubeClusterListRoundTrip(t *testing.T) {
	in := []fleet.KubeCluster{filled[fleet.KubeCluster](t), {ID: "second"}}
	in[0].Image.GatewayCredential = ""
	requireEqual(t, KubeClustersFromProto(KubeClustersToProto(in)), in)
	if KubeClustersToProto(nil) != nil || KubeClustersFromProto(nil) != nil {
		t.Error("empty cluster lists should map to nil both ways")
	}
	if got := KubeClusterFromProto(nil); !reflect.DeepEqual(got, fleet.KubeCluster{}) {
		t.Errorf("nil cluster proto should map to the zero value, got %+v", got)
	}
}

// An unset setting stays unset both ways: the tri-states stay nil (a nil
// reads as the default, an explicit false must not), zero times stay absent.
func TestKubeClusterUnsetSettingsStayUnset(t *testing.T) {
	pk := KubeClusterToProto(fleet.KubeCluster{ID: "c"})
	if pk.AutoExpandDisk != nil || pk.AllowSharedBuildkit != nil || pk.BlockMetadata != nil ||
		pk.AllowPrivileged != nil || pk.AutomationMaxPerAgent != nil ||
		pk.GetImage().ManagedPullSecret != nil || pk.GetImage().UploadPacePercent != nil {
		t.Fatalf("unset tri-states must be absent from the wire: %+v", pk)
	}
	if pk.CreatedAt != nil || pk.UpdatedAt != nil || pk.VerifiedAt != nil {
		t.Fatalf("zero times must be absent from the wire: %+v", pk)
	}
	if back := KubeClusterFromProto(pk); !reflect.DeepEqual(back, fleet.KubeCluster{ID: "c"}) {
		t.Fatalf("unset record read back as %+v", back)
	}

	off, zero := false, 0
	set := fleet.KubeCluster{AllowPrivileged: &off, AutomationMaxPerAgent: &zero}
	pk = KubeClusterToProto(set)
	if pk.AllowPrivileged == nil || pk.GetAllowPrivileged() || pk.AutomationMaxPerAgent == nil || pk.GetAutomationMaxPerAgent() != 0 {
		t.Fatalf("explicit false / 0 lost on the wire: %+v", pk)
	}
	back := KubeClusterFromProto(pk)
	if back.AllowPrivileged == nil || *back.AllowPrivileged || back.AutomationMaxPerAgent == nil || *back.AutomationMaxPerAgent != 0 {
		t.Fatalf("explicit false / 0 lost on the way back: %+v", back)
	}
	if back.AllowPrivileged == set.AllowPrivileged {
		t.Fatal("the converted record aliases the original's pointer")
	}
}

// A converted record shares nothing with its source, either way: changing the
// stored record (or a received one) after converting it changes no copy.
func TestKubeClusterConversionSharesNothing(t *testing.T) {
	k := filled[fleet.KubeCluster](t)
	pk := KubeClusterToProto(k)
	k.KubeconfigPaths[0], k.ExtraPath[0], k.PluginEnv[0], k.Image.PullSecrets[0] = "changed", "changed", "changed", "changed"
	k.NodeSelector["added"] = "changed"
	if pk.GetKubeconfigPaths()[0] == "changed" || pk.GetExtraPath()[0] == "changed" || pk.GetPluginEnv()[0] == "changed" ||
		pk.GetImage().GetPullSecrets()[0] == "changed" || pk.GetNodeSelector()["added"] != "" {
		t.Fatalf("the proto aliases the record it was converted from: %+v", pk)
	}

	back := KubeClusterFromProto(pk)
	pk.KubeconfigPaths[0], pk.ExtraPath[0], pk.PluginEnv[0], pk.Image.PullSecrets[0] = "changed", "changed", "changed", "changed"
	pk.NodeSelector["added"] = "changed"
	if back.KubeconfigPaths[0] == "changed" || back.ExtraPath[0] == "changed" || back.PluginEnv[0] == "changed" ||
		back.Image.PullSecrets[0] == "changed" || back.NodeSelector["added"] != "" {
		t.Fatalf("the record aliases the proto it was read from: %+v", back)
	}
}

// The gateway image credential is never returned to a client and never set
// by one: the proto has no field for it anywhere in the record, and a record
// that holds one sends no trace of it.
func TestKubeClusterNeverCarriesTheGatewayCredential(t *testing.T) {
	var walk func(protoreflect.MessageDescriptor, map[protoreflect.FullName]bool)
	walk = func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if fd.Name() == "gateway_credential" {
				t.Errorf("%s has a gateway_credential field: it must never reach a client", md.FullName())
			}
			if fd.Message() != nil {
				walk(fd.Message(), seen)
			}
		}
	}
	walk((&fleetgrpc.KubeClusters{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})

	k := filled[fleet.KubeCluster](t)
	k.Image.GatewayCredential = "gw-credential-sentinel"
	wire, err := proto.Marshal(KubeClusterToProto(k))
	if err != nil {
		t.Fatal(err)
	}
	asJSON, err := protojson.Marshal(KubeClusterToProto(k))
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{wire, asJSON} {
		if bytes.Contains(encoded, []byte("gw-credential-sentinel")) {
			t.Fatal("the gateway credential reached the wire")
		}
	}
}

// TestKubeClusterWirePlacement pins each domain field to its proto field (the
// round trip can't see a symmetric swap). String fields come from the filler,
// which makes each one "v:KubeCluster.<path>", so a swap reads its
// neighbour's value.
func TestKubeClusterWirePlacement(t *testing.T) {
	pinArity[fleet.KubeCluster](t, 49)
	pinArity[fleet.KubeResources](t, 3)
	pinArity[fleet.KubeToleration](t, 5)
	pinArity[fleet.KubeCacheSizes](t, 3)
	pinArity[fleet.KubeServiceImages](t, 5)
	pinArity[fleet.KubeImageSettings](t, 10)

	k := filled[fleet.KubeCluster](t)
	k.MaxInstances = 3
	maxPerAgent, pace, seconds := 4, 5, int64(8)
	k.AutomationMaxPerAgent = &maxPerAgent
	k.Image.UploadPacePercent = &pace
	k.Tolerations[0].TolerationSeconds = &seconds
	k.Revision = 6
	base := time.Unix(1720000000, 0).UTC()
	k.CreatedAt, k.UpdatedAt, k.VerifiedAt = base, base.Add(time.Hour), base.Add(2*time.Hour)
	pk := KubeClusterToProto(k)

	strs := map[string]string{
		"ID":                             pk.GetId(),
		"DisplayName":                    pk.GetDisplayName(),
		"Context":                        pk.GetContext(),
		"Namespace":                      pk.GetNamespace(),
		"Server":                         pk.GetServer(),
		"ClusterUID":                     pk.GetClusterUid(),
		"KubeconfigPaths[0]":             pk.GetKubeconfigPaths()[0],
		"ExtraPath[0]":                   pk.GetExtraPath()[0],
		"PluginEnv[0]":                   pk.GetPluginEnv()[0],
		"StorageClass":                   pk.GetStorageClass(),
		"DiskSize":                       pk.GetDiskSize(),
		"SnapshotClass":                  pk.GetSnapshotClass(),
		"CloneMethod":                    pk.GetCloneMethod(),
		"CloneConsistency":               pk.GetCloneConsistency(),
		"SharedStorageClass":             pk.GetSharedStorageClass(),
		"ShareSize":                      pk.GetShareSize(),
		"ShareRetention":                 pk.GetShareRetention(),
		"CredentialScope":                pk.GetCredentialScope(),
		"CacheStorageClass":              pk.GetCacheStorageClass(),
		"CacheSizes.Buildkit":            pk.GetCacheSizes().GetBuildkit(),
		"CacheSizes.DebCache":            pk.GetCacheSizes().GetDebcache(),
		"CacheSizes.ImageCache":          pk.GetCacheSizes().GetImagecache(),
		"ServiceImages.Buildkit":         pk.GetServiceImages().GetBuildkit(),
		"ServiceImages.BuildkitRootless": pk.GetServiceImages().GetBuildkitRootless(),
		"ServiceImages.DebCache":         pk.GetServiceImages().GetDebcache(),
		"ServiceImages.ImageCache":       pk.GetServiceImages().GetImagecache(),
		"ServiceImages.DinD":             pk.GetServiceImages().GetDind(),
		"DinDAddressPool":                pk.GetDindAddressPool(),
		"Resources.CPURequest":           pk.GetResources().GetCpuRequest(),
		"Resources.MemoryRequest":        pk.GetResources().GetMemoryRequest(),
		"Resources.AutopilotMemory":      pk.GetResources().GetAutopilotMemory(),
		"MemoryLimit":                    pk.GetMemoryLimit(),
		"NodeSelector.key":               firstKey(pk.GetNodeSelector()),
		"NodeSelector.val":               pk.GetNodeSelector()[firstKey(pk.GetNodeSelector())],
		"Tolerations[0].Key":             pk.GetTolerations()[0].GetKey(),
		"Tolerations[0].Operator":        pk.GetTolerations()[0].GetOperator(),
		"Tolerations[0].Value":           pk.GetTolerations()[0].GetValue(),
		"Tolerations[0].Effect":          pk.GetTolerations()[0].GetEffect(),
		"DebugCapabilities":              pk.GetDebugCapabilities(),
		"RuntimeClass":                   pk.GetRuntimeClass(),
		"ServiceAccount":                 pk.GetServiceAccount(),
		"PodOverridesYAML":               pk.GetPodOverridesYaml(),
		"HostGateway":                    pk.GetHostGateway(),
		"MaxTotalDisk":                   pk.GetMaxTotalDisk(),
		"AutomationFireInterval":         pk.GetAutomationFireInterval(),
		"Image.Mode":                     pk.GetImage().GetMode(),
		"Image.Registry":                 pk.GetImage().GetRegistry(),
		"Image.PullSecrets[0]":           pk.GetImage().GetPullSecrets()[0],
		"Image.Builder":                  pk.GetImage().GetBuilder(),
		"Image.Platform":                 pk.GetImage().GetPlatform(),
		"Image.Retention":                pk.GetImage().GetRetention(),
		"RebuildExport":                  pk.GetRebuildExport(),
	}
	for path, got := range strs {
		if want := "v:KubeCluster." + path; got != want {
			t.Errorf("%s landed in the wrong proto field: got %q", path, got)
		}
	}

	if pk.GetMaxInstances() != 3 || pk.GetAutomationMaxPerAgent() != 4 || pk.GetImage().GetUploadPacePercent() != 5 ||
		pk.GetRevision() != 6 || pk.GetTolerations()[0].GetTolerationSeconds() != 8 {
		t.Errorf("number wire placement wrong: %+v", pk)
	}
	if !pk.GetCreatedAt().AsTime().Equal(k.CreatedAt) || !pk.GetUpdatedAt().AsTime().Equal(k.UpdatedAt) ||
		!pk.GetVerifiedAt().AsTime().Equal(k.VerifiedAt) {
		t.Errorf("time wire placement wrong: %+v", pk)
	}

	// Bools can't be made mutually distinct: set one per iteration and assert
	// ITS getter reads true.
	plain := []struct {
		name string
		set  func(*fleet.KubeCluster)
		get  func(*fleetgrpc.KubeCluster) bool
	}{
		{"BackupWorkspaceOnStop", func(k *fleet.KubeCluster) { k.BackupWorkspaceOnStop = true }, (*fleetgrpc.KubeCluster).GetBackupWorkspaceOnStop},
		{"NodeLocalAck", func(k *fleet.KubeCluster) { k.NodeLocalAck = true }, (*fleetgrpc.KubeCluster).GetNodeLocalAck},
		{"DeleteCacheVolumesOnDestroy", func(k *fleet.KubeCluster) { k.DeleteCacheVolumesOnDestroy = true }, (*fleetgrpc.KubeCluster).GetDeleteCacheVolumesOnDestroy},
		{"ProtectFromDrain", func(k *fleet.KubeCluster) { k.ProtectFromDrain = true }, (*fleetgrpc.KubeCluster).GetProtectFromDrain},
		{"Image.InsecureRegistry", func(k *fleet.KubeCluster) { k.Image.InsecureRegistry = true }, func(p *fleetgrpc.KubeCluster) bool { return p.GetImage().GetInsecureRegistry() }},
	}
	for _, b := range plain {
		var k fleet.KubeCluster
		b.set(&k)
		if !b.get(KubeClusterToProto(k)) {
			t.Errorf("bool %s landed in the wrong proto field", b.name)
		}
	}

	// The tri-states the same way, by presence: only the one set is present.
	yes := true
	triStates := []struct {
		name string
		set  func(*fleet.KubeCluster)
		get  func(*fleetgrpc.KubeCluster) *bool
	}{
		{"AutoExpandDisk", func(k *fleet.KubeCluster) { k.AutoExpandDisk = &yes }, func(p *fleetgrpc.KubeCluster) *bool { return p.AutoExpandDisk }},
		{"AllowSharedBuildkit", func(k *fleet.KubeCluster) { k.AllowSharedBuildkit = &yes }, func(p *fleetgrpc.KubeCluster) *bool { return p.AllowSharedBuildkit }},
		{"BlockMetadata", func(k *fleet.KubeCluster) { k.BlockMetadata = &yes }, func(p *fleetgrpc.KubeCluster) *bool { return p.BlockMetadata }},
		{"AllowPrivileged", func(k *fleet.KubeCluster) { k.AllowPrivileged = &yes }, func(p *fleetgrpc.KubeCluster) *bool { return p.AllowPrivileged }},
		{"Image.ManagedPullSecret", func(k *fleet.KubeCluster) { k.Image.ManagedPullSecret = &yes }, func(p *fleetgrpc.KubeCluster) *bool { return p.GetImage().ManagedPullSecret }},
	}
	for _, ts := range triStates {
		var k fleet.KubeCluster
		ts.set(&k)
		pk := KubeClusterToProto(k)
		for _, other := range triStates {
			got := other.get(pk)
			if other.name == ts.name && (got == nil || !*got) {
				t.Errorf("tri-state %s landed in the wrong proto field", ts.name)
			}
			if other.name != ts.name && got != nil {
				t.Errorf("setting %s set %s on the wire", ts.name, other.name)
			}
		}
	}
}

// firstKey returns m's only key ("" when empty).
func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}
