package fleet

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestKubeClusterGetDisplayName(t *testing.T) {
	if got := (&KubeCluster{ID: "c1"}).GetDisplayName(); got != "c1" {
		t.Errorf("GetDisplayName() without a display name = %q, want the ID", got)
	}
	if got := (&KubeCluster{ID: "c1", DisplayName: "prod-eu"}).GetDisplayName(); got != "prod-eu" {
		t.Errorf("GetDisplayName() = %q, want prod-eu", got)
	}
}

// An unset tri-state takes its default; a set one wins either way.
func TestKubeClusterTriStateDefaults(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		get  func(*KubeCluster) bool
		set  func(*KubeCluster, *bool)
		def  bool
	}{
		{"AutoExpandsDisk", (*KubeCluster).AutoExpandsDisk, func(k *KubeCluster, b *bool) { k.AutoExpandDisk = b }, true},
		{"AllowsSharedBuildkit", (*KubeCluster).AllowsSharedBuildkit, func(k *KubeCluster, b *bool) { k.AllowSharedBuildkit = b }, true},
		{"BlocksMetadata", (*KubeCluster).BlocksMetadata, func(k *KubeCluster, b *bool) { k.BlockMetadata = b }, true},
		{"ManagesPullSecret", func(k *KubeCluster) bool { return k.Image.ManagesPullSecret() }, func(k *KubeCluster, b *bool) { k.Image.ManagedPullSecret = b }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var k KubeCluster
			if got := tt.get(&k); got != tt.def {
				t.Errorf("unset = %v, want the default %v", got, tt.def)
			}
			tt.set(&k, &yes)
			if !tt.get(&k) {
				t.Error("set true reads false")
			}
			tt.set(&k, &no)
			if tt.get(&k) {
				t.Error("set false reads true")
			}
		})
	}
}

func TestKubeClusterEnumsValid(t *testing.T) {
	type validator interface{ Valid() bool }
	tests := []struct {
		name  string
		valid []validator
		bogus validator
	}{
		{"clone method", []validator{KubeCloneMethod(""), KubeCloneAuto, KubeCloneSnapshot, KubeCloneCSIClone, KubeCloneCopy}, KubeCloneMethod("rsync")},
		{"clone consistency", []validator{KubeCloneConsistency(""), KubeCloneConsistencyAuto, KubeCloneStopSource}, KubeCloneConsistency("freeze")},
		{"share retention", []validator{KubeShareRetention(""), KubeShareDelete, KubeShareRetain}, KubeShareRetention("keep")},
		{"credential scope", []validator{KubeCredentialScope(""), KubeCredentialsPerLocation, KubeCredentialsPerFleet}, KubeCredentialScope("global")},
		{"debug capabilities", []validator{KubeDebugCapabilities(""), KubeDebugCapabilitiesAuto, KubeDebugCapabilitiesRequire}, KubeDebugCapabilities("drop")},
		{"host gateway", []validator{KubeHostGateway(""), KubeHostGatewayAuto, KubeHostGatewayOff}, KubeHostGateway("on")},
		{"image mode", []validator{KubeImageMode(""), KubeImageAuto, KubeImageNodeLoad, KubeImageRegistry, KubeImageGateway}, KubeImageMode("push")},
	}
	for _, tt := range tests {
		for _, v := range tt.valid {
			if !v.Valid() {
				t.Errorf("%s %q should be valid", tt.name, v)
			}
		}
		if tt.bogus.Valid() {
			t.Errorf("%s %q should be invalid", tt.name, tt.bogus)
		}
	}
}

// The record is persisted as JSON (kube_clusters.json): every field, the
// gateway credential included, must survive the file, under the keys the
// proto names.
func TestKubeClusterJSONRoundTrip(t *testing.T) {
	off := false
	four, eighty := 4, 80
	seconds := int64(300)
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	in := KubeCluster{
		ID: "c1", DisplayName: "prod-eu", Context: "gke_acme", KubeconfigPaths: []string{"/home/ben/.kube/config"},
		Namespace: "fleet", Server: "https://34.90.12.7", ClusterUID: "uid", ExtraPath: []string{"/opt/gcloud/bin"},
		PluginEnv: []string{"CLOUDSDK_CONFIG"}, StorageClass: "standard-rwo", DiskSize: "60Gi", AutoExpandDisk: &off,
		SnapshotClass: "snap", CloneMethod: KubeCloneSnapshot, CloneConsistency: KubeCloneStopSource,
		BackupWorkspaceOnStop: true, NodeLocalAck: true, SharedStorageClass: "filestore", ShareSize: "1Ti",
		ShareRetention: KubeShareRetain, CredentialScope: KubeCredentialsPerFleet, CacheStorageClass: "cache",
		CacheSizes:                  KubeCacheSizes{Buildkit: "1Gi", DebCache: "2Gi", ImageCache: "3Gi"},
		ServiceImages:               KubeServiceImages{Buildkit: "b", BuildkitRootless: "br", DebCache: "d", ImageCache: "i", DinD: "dd"},
		AllowSharedBuildkit:         &off,
		DeleteCacheVolumesOnDestroy: true, DinDAddressPool: "10.10.0.0/16", BlockMetadata: &off,
		Resources:       KubeResources{CPURequest: "1", MemoryRequest: "4Gi", AutopilotMemory: "6Gi"},
		MemoryLimit:     "16Gi",
		NodeSelector:    map[string]string{"pool": "fleet"},
		Tolerations:     []KubeToleration{{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoSchedule", TolerationSeconds: &seconds}},
		AllowPrivileged: &off, DebugCapabilities: KubeDebugCapabilitiesRequire, RuntimeClass: "sysbox-runc",
		ServiceAccount: "fleet", PodOverridesYAML: "spec: {}", ProtectFromDrain: true, HostGateway: KubeHostGatewayOff,
		MaxInstances: 20, MaxTotalDisk: "1Ti", AutomationFireInterval: "1m", AutomationMaxPerAgent: &four,
		Image: KubeImageSettings{
			Mode: KubeImageRegistry, Registry: "reg.example.com/team", InsecureRegistry: true, PullSecrets: []string{"regcred"},
			ManagedPullSecret: &off, GatewayCredential: "secret", Builder: "fleet", Platform: "linux/arm64",
			Retention: "24h", UploadPacePercent: &eighty,
		},
		RebuildExport: "auto", CreatedAt: at, UpdatedAt: at.Add(time.Hour), VerifiedAt: at.Add(2 * time.Hour), Revision: 9,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out KubeCluster
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("JSON round trip lost data:\n got: %+v\nwant: %+v", out, in)
	}
	for _, key := range []string{`"kubeconfig_paths"`, `"cluster_uid"`, `"pod_overrides_yaml"`, `"dind_address_pool"`, `"gateway_credential"`, `"automation_max_per_agent"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("persisted JSON has no %s key: %s", key, data)
		}
	}
}

// The automation limits take their defaults when unset, and 0 turns either
// off.
func TestKubeClusterAutomationLimits(t *testing.T) {
	zero, two, neg := 0, 2, -1
	for _, tc := range []struct {
		interval string
		max      *int
		wantI    time.Duration
		wantMax  int
	}{
		{"", nil, DefaultKubeAutomationFireInterval, DefaultKubeAutomationMaxPerAgent},
		{"0", &zero, 0, 0},
		{"45s", &two, 45 * time.Second, 2},
		{"bogus", &neg, DefaultKubeAutomationFireInterval, 0},
	} {
		k := &KubeCluster{AutomationFireInterval: tc.interval, AutomationMaxPerAgent: tc.max}
		if got := k.AutomationInterval(); got != tc.wantI {
			t.Errorf("AutomationInterval(%q) = %v, want %v", tc.interval, got, tc.wantI)
		}
		if got := k.AutomationRunLimit(); got != tc.wantMax {
			t.Errorf("AutomationRunLimit(%v) = %d, want %d", tc.max, got, tc.wantMax)
		}
	}
}
