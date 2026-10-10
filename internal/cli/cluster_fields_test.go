package cli

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

func ptrTo[T any](v T) *T { return &v }

func TestParseClusterFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		want  *fleetgrpc.KubeCluster
		paths []string
	}{
		{"string", []string{"storage_class=fast"}, &fleetgrpc.KubeCluster{StorageClass: "fast"}, []string{"storage_class"}},
		{"alias and dashes", []string{"name=prod-eu", "Disk-Size=80Gi"}, &fleetgrpc.KubeCluster{DisplayName: "prod-eu", DiskSize: "80Gi"}, []string{"display_name", "disk_size"}},
		{"bool", []string{"protect_from_drain=true"}, &fleetgrpc.KubeCluster{ProtectFromDrain: true}, []string{"protect_from_drain"}},
		{"optional bool false is set", []string{"allow_privileged=false"}, &fleetgrpc.KubeCluster{AllowPrivileged: ptrTo(false)}, []string{"allow_privileged"}},
		{"int32", []string{"max_instances=20"}, &fleetgrpc.KubeCluster{MaxInstances: 20}, []string{"max_instances"}},
		{"optional int32 zero is set", []string{"automation_max_per_agent=0"}, &fleetgrpc.KubeCluster{AutomationMaxPerAgent: ptrTo(int32(0))}, []string{"automation_max_per_agent"}},
		{"list item per occurrence", []string{"extra_path=/opt/a", "plugin_env=AWS_PROFILE", "extra_path=/opt/b"},
			&fleetgrpc.KubeCluster{ExtraPath: []string{"/opt/a", "/opt/b"}, PluginEnv: []string{"AWS_PROFILE"}}, []string{"extra_path", "plugin_env"}},
		{"map entry per occurrence", []string{"node_selector=pool=fleet", "node_selector=zone=a=b"},
			&fleetgrpc.KubeCluster{NodeSelector: map[string]string{"pool": "fleet", "zone": "a=b"}}, []string{"node_selector"}},
		{"sub-field", []string{"resources.cpu_request=1", "image.mode=registry", "image.upload_pace_percent=50"},
			&fleetgrpc.KubeCluster{Resources: &fleetgrpc.KubeResources{CpuRequest: "1"}, Image: &fleetgrpc.KubeImageSettings{Mode: "registry", UploadPacePercent: ptrTo(int32(50))}},
			[]string{"resources.cpu_request", "image.mode", "image.upload_pace_percent"}},
		{"message as JSON", []string{`resources={"cpu_request":"2","memory_request":"4Gi"}`},
			&fleetgrpc.KubeCluster{Resources: &fleetgrpc.KubeResources{CpuRequest: "2", MemoryRequest: "4Gi"}}, []string{"resources"}},
		{"tolerations", []string{"tolerations=dedicated=fleet:NoExecute:60", "tolerations=gpu", "tolerations=:NoSchedule", "tolerations=spot=:PreferNoSchedule:2m",
			`tolerations={"operator":"Exists"}`},
			&fleetgrpc.KubeCluster{Tolerations: []*fleetgrpc.KubeToleration{
				{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoExecute", TolerationSeconds: ptrTo(int64(60))},
				{Key: "gpu", Operator: "Exists"},
				{Operator: "Exists", Effect: "NoSchedule"},
				{Key: "spot", Operator: "Equal", Effect: "PreferNoSchedule", TolerationSeconds: ptrTo(int64(120))},
				{Operator: "Exists"},
			}}, []string{"tolerations"}},
		{"clear", []string{"storage_class=", "allow_privileged=", "extra_path=", "node_selector=", "resources=", "image.mode="},
			&fleetgrpc.KubeCluster{Image: &fleetgrpc.KubeImageSettings{}},
			[]string{"storage_class", "allow_privileged", "extra_path", "node_selector", "resources", "image.mode"}},
		{"quoted", []string{`pod_overrides_yaml="priorityClassName: fleet\nhostNetwork: false\n"`},
			&fleetgrpc.KubeCluster{PodOverridesYaml: "priorityClassName: fleet\nhostNetwork: false\n"}, []string{"pod_overrides_yaml"}},
		{"multi-line as given", []string{"pod_overrides_yaml=a: 1\nb: 2"}, &fleetgrpc.KubeCluster{PodOverridesYaml: "a: 1\nb: 2"}, []string{"pod_overrides_yaml"}},
		{"value with equals", []string{"plugin_env=AWS_REGION=eu-west-1"}, &fleetgrpc.KubeCluster{PluginEnv: []string{"AWS_REGION=eu-west-1"}}, []string{"plugin_env"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, paths, err := parseClusterFields(tc.args)
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if !proto.Equal(got, tc.want) {
				t.Errorf("parse %v =\n %v\nwant\n %v", tc.args, got, tc.want)
			}
			if strings.Join(paths, ",") != strings.Join(tc.paths, ",") {
				t.Errorf("paths = %v, want %v", paths, tc.paths)
			}
		})
	}
}

func TestParseClusterFieldsErrors(t *testing.T) {
	for _, tc := range []struct {
		arg, err string
	}{
		{"storage-class", "not <field>=<value>"},
		{"=x", "not <field>=<value>"},
		{"nope=1", `unknown field "nope"`},
		{"image.nope=1", `unknown field "image.nope"`},
		{"disk_size.x=1", "disk_size has no sub-fields"},
		{"tolerations.key=a", "tolerations has no sub-fields"},
		{"id=forged", "id is set by fleetd"},
		{"cluster_uid=x", "cluster_uid is set by fleetd"},
		{"revision=3", "revision is set by fleetd"},
		{"created_at.seconds=1", "created_at is set by fleetd"},
		{"allow_privileged=maybe", `allow_privileged: "maybe" is not true or false`},
		{"max_instances=ten", `max_instances: "ten" is not a whole number`},
		{"node_selector=pool", `node_selector: "pool" is not <key>=<value>`},
		{"resources=1", "set its sub-fields (resources.cpu_request, resources.memory_request, resources.autopilot_memory)"},
		{"resources={bad", "resources:"},
		{"tolerations=a:b:c:d", "is not <key>[=<value>][:<effect>[:<seconds>]]"},
		{"tolerations=a:NoExecute:soon", `"soon" is not a number of seconds`},
	} {
		if _, _, err := parseClusterFields([]string{tc.arg}); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("parse %q = %v, want %q", tc.arg, err, tc.err)
		}
	}
	for _, tc := range []struct {
		args []string
		err  string
	}{
		{[]string{"disk_size=1Gi", "disk_size=2Gi"}, "disk_size given more than once"},
		{[]string{"node_selector=a=1", "node_selector=a=2"}, `node_selector: key "a" given twice`},
		{[]string{"extra_path=/a", "extra_path="}, "extra_path: an empty item"},
		{[]string{"resources={}", "resources={}"}, "resources given more than once"},
	} {
		if _, _, err := parseClusterFields(tc.args); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("parse %v = %v, want %q", tc.args, err, tc.err)
		}
	}
}

// everyClusterField is a KubeCluster with a non-default value in every field
// a client may set.
func everyClusterField() *fleetgrpc.KubeCluster {
	return &fleetgrpc.KubeCluster{
		DisplayName: "every-field", Context: "arn:aws:eks:eu-west-1:1:cluster/prod", KubeconfigPaths: []string{"/home/me/.kube/config", "/etc/kube/extra"},
		Namespace: "fleet", ExtraPath: []string{"/opt/sdk/bin", "/usr/local/aws/bin"}, PluginEnv: []string{"AWS_PROFILE", "AWS_REGION=eu-west-1"},
		StorageClass: "standard", DiskSize: "80Gi", AutoExpandDisk: ptrTo(false), SnapshotClass: "csi-snap", CloneMethod: "snapshot",
		CloneConsistency: "stop-source", BackupWorkspaceOnStop: true, NodeLocalAck: true, SharedStorageClass: "nfs", ShareSize: "20Gi",
		ShareRetention: "retain", CredentialScope: "fleet", CacheStorageClass: "fast",
		CacheSizes: &fleetgrpc.KubeCacheSizes{Buildkit: "30Gi", Debcache: "15Gi", Imagecache: "25Gi"},
		ServiceImages: &fleetgrpc.KubeServiceImages{Buildkit: "moby/buildkit:v0.34.0", BuildkitRootless: "moby/buildkit:v0.34.0-rootless",
			Debcache: "sameersbn/apt-cacher-ng:3.7", Imagecache: "registry:3", Dind: "docker:29-dind"},
		AllowSharedBuildkit: ptrTo(false), DeleteCacheVolumesOnDestroy: true, DindAddressPool: "10.200.0.0/16", BlockMetadata: ptrTo(false),
		Resources:   &fleetgrpc.KubeResources{CpuRequest: "1", MemoryRequest: "3Gi", AutopilotMemory: "6Gi"},
		MemoryLimit: "12Gi", NodeSelector: map[string]string{"pool": "fleet", "topology.kubernetes.io/zone": "eu-west-1a"},
		Tolerations: []*fleetgrpc.KubeToleration{
			{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoExecute", TolerationSeconds: ptrTo(int64(60))},
			{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"},
			{Operator: "Exists"},
		},
		AllowPrivileged: ptrTo(true), DebugCapabilities: "require", RuntimeClass: "sysbox-runc", ServiceAccount: "fleet-pods",
		PodOverridesYaml: "priorityClassName: fleet\nterminationGracePeriodSeconds: 5\n", ProtectFromDrain: true, HostGateway: "off",
		MaxInstances: 20, MaxTotalDisk: "1Ti", AutomationFireInterval: "45s", AutomationMaxPerAgent: ptrTo(int32(2)),
		Image: &fleetgrpc.KubeImageSettings{Mode: "registry", Registry: "registry.example.com/team", InsecureRegistry: true,
			PullSecrets: []string{"regcred", "other"}, ManagedPullSecret: ptrTo(false), Builder: "fleet", Platform: "linux/arm64",
			Retention: "48h", UploadPacePercent: ptrTo(int32(50))},
		RebuildExport: "tar",
	}
}

// The fixture sets every settable field, so a field added to KubeCluster is
// caught here until the round trip below covers it.
func TestEveryClusterFieldFixtureIsComplete(t *testing.T) {
	var check func(prefix string, m protoreflect.Message)
	check = func(prefix string, m protoreflect.Message) {
		fields := m.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			if prefix == "" && daemonOwnedClusterFields[string(fd.Name())] {
				continue
			}
			if !m.Has(fd) {
				t.Errorf("everyClusterField leaves %s%s unset", prefix, fd.Name())
				continue
			}
			if fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
				check(prefix+string(fd.Name())+".", m.Get(fd).Message())
			}
		}
	}
	check("", everyClusterField().ProtoReflect())
}

// `ls <cluster>` prints a record as `set` reads it: every field survives
// the trip, whatever its kind.
func TestClusterFieldLinesRoundTrip(t *testing.T) {
	want := everyClusterField()
	lines := clusterFieldLines(want)
	got, _, err := parseClusterFields(lines)
	if err != nil {
		t.Fatalf("parse the printed record: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if !proto.Equal(got, want) {
		t.Errorf("round trip =\n %v\nwant\n %v\nlines:\n%s", got, want, strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if strings.Contains(line, "\n") {
			t.Errorf("a printed field spans lines: %q", line)
		}
	}
	for _, want := range []string{
		"tolerations=dedicated=fleet:NoExecute:60", "tolerations=gpu:NoSchedule", `tolerations={"operator":"Exists"}`,
		"node_selector=pool=fleet", "extra_path=/opt/sdk/bin", "image.mode=registry", "allow_privileged=true",
		`pod_overrides_yaml="priorityClassName: fleet\nterminationGracePeriodSeconds: 5\n"`,
	} {
		if !strings.Contains(strings.Join(lines, "\n")+"\n", want+"\n") {
			t.Errorf("no line %q in\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// The daemon's bookkeeping prints too, but never parses back.
func TestClusterFieldLinesPrintsBookkeeping(t *testing.T) {
	lines := strings.Join(clusterFieldLines(&fleetgrpc.KubeCluster{Id: "prod-a1b2c3", Revision: 4}), "\n")
	if lines != "id=prod-a1b2c3\nrevision=4" {
		t.Errorf("lines = %q", lines)
	}
}

func TestClusterFieldHelpListsEverySettableField(t *testing.T) {
	help := " " + strings.Join(strings.Fields(clusterFieldHelp()), " ") + " "
	for _, line := range clusterFieldLines(everyClusterField()) {
		field, _, _ := strings.Cut(line, "=")
		if !strings.Contains(help, " "+field+" ") {
			t.Errorf("set --help does not list %s", field)
		}
	}
	for name := range daemonOwnedClusterFields {
		if strings.Contains(help, " "+name+" ") {
			t.Errorf("set --help lists the daemon-owned %s", name)
		}
	}
}

// The CLI's daemon-owned list is the proto's bookkeeping: id, server,
// cluster_uid, the timestamps and revision.
func TestDaemonOwnedClusterFields(t *testing.T) {
	fields := (&fleetgrpc.KubeCluster{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		isTimestamp := fd.Message() != nil && fd.Message().FullName() == timestampFullName
		if isTimestamp && !daemonOwnedClusterFields[string(fd.Name())] {
			t.Errorf("%s is bookkeeping but settable", fd.Name())
		}
	}
	for name := range daemonOwnedClusterFields {
		if fields.ByName(protoreflect.Name(name)) == nil {
			t.Errorf("daemon-owned %s is not a KubeCluster field", name)
		}
	}
}
