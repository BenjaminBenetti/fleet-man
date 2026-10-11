//go:build !nok8s

package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// everyKubeField is a KubeCluster with a valid, non-default value in every
// field a client may set. extraPath must be an absolute directory.
func everyKubeField(extraPath string) *fleetgrpc.KubeCluster {
	return &fleetgrpc.KubeCluster{
		DisplayName:           "every-field",
		Context:               "minikube",
		KubeconfigPaths:       nil, // set by the caller
		Namespace:             "fleet",
		ExtraPath:             []string{extraPath},
		PluginEnv:             []string{"AWS_PROFILE", "AWS_REGION=eu-west-1"},
		StorageClass:          "standard",
		DiskSize:              "80Gi",
		AutoExpandDisk:        ptr(false),
		SnapshotClass:         "csi-hostpath-snapclass",
		CloneMethod:           "snapshot",
		CloneConsistency:      "stop-source",
		BackupWorkspaceOnStop: true,
		NodeLocalAck:          true,
		SharedStorageClass:    "nfs",
		ShareSize:             "20Gi",
		ShareRetention:        "retain",
		CredentialScope:       "fleet",
		CacheStorageClass:     "fast",
		CacheSizes:            &fleetgrpc.KubeCacheSizes{Buildkit: "30Gi", Debcache: "15Gi", Imagecache: "25Gi"},
		ServiceImages: &fleetgrpc.KubeServiceImages{
			Buildkit: "moby/buildkit:v0.34.0", BuildkitRootless: "moby/buildkit:v0.34.0-rootless",
			Debcache: "sameersbn/apt-cacher-ng:3.7", Imagecache: "registry:3", Dind: "docker:29-dind",
		},
		AllowSharedBuildkit:         ptr(false),
		DeleteCacheVolumesOnDestroy: true,
		DindAddressPool:             "10.200.0.0/16",
		BlockMetadata:               ptr(false),
		Resources:                   &fleetgrpc.KubeResources{CpuRequest: "1", MemoryRequest: "3Gi", AutopilotMemory: "6Gi"},
		MemoryLimit:                 "12Gi",
		NodeSelector:                map[string]string{"pool": "fleet"},
		Tolerations: []*fleetgrpc.KubeToleration{
			{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoExecute", TolerationSeconds: ptr(int64(60))},
		},
		AllowPrivileged:        ptr(true),
		DebugCapabilities:      "require",
		RuntimeClass:           "sysbox-runc",
		ServiceAccount:         "fleet-pods",
		PodOverridesYaml:       "priorityClassName: fleet\n",
		ProtectFromDrain:       true,
		HostGateway:            "off",
		MaxInstances:           20,
		MaxTotalDisk:           "1Ti",
		AutomationFireInterval: "45s",
		AutomationMaxPerAgent:  ptr(int32(2)),
		Image: &fleetgrpc.KubeImageSettings{
			Mode: "registry", Registry: "registry.example.com/team", InsecureRegistry: true,
			PullSecrets: []string{"regcred"}, ManagedPullSecret: ptr(false), Builder: "fleet",
			Platform: "linux/arm64", Retention: "48h", UploadPacePercent: ptr(int32(50)),
		},
		RebuildExport: "tar",
	}
}

// settableKubePaths lists every KubeCluster field a client may set.
func settableKubePaths() []string {
	var paths []string
	fields := (&fleetgrpc.KubeCluster{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if name := string(fields.Get(i).Name()); !daemonOwnedPaths[name] {
			paths = append(paths, name)
		}
	}
	return paths
}

// The fixture sets every settable field, so a field added to KubeCluster
// later is caught here until the remote-parity test covers it too.
func TestEveryKubeFieldFixtureIsComplete(t *testing.T) {
	c := everyKubeField("/opt/sdk/bin")
	c.KubeconfigPaths = []string{"/home/me/.kube/config"}
	m := c.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if daemonOwnedPaths[string(fd.Name())] {
			continue
		}
		if !m.Has(fd) {
			t.Errorf("everyKubeField leaves %s unset", fd.Name())
		}
		if fd.Message() != nil && !fd.IsList() && !fd.IsMap() {
			sub := m.Get(fd).Message()
			subFields := sub.Descriptor().Fields()
			for j := 0; j < subFields.Len(); j++ {
				if !sub.Has(subFields.Get(j)) {
					t.Errorf("everyKubeField leaves %s.%s unset", fd.Name(), subFields.Get(j).Name())
				}
			}
		}
	}
}

func TestAddKubeClusterAppliesDefaultsAndPins(t *testing.T) {
	f := newKubeFixture(t)
	st := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Id: "chosen-by-client", ClusterUid: "forged", Server: "https://forged", Revision: 99})
	c := st.GetCluster()
	if !strings.HasPrefix(c.GetId(), "minikube-") || names.ValidateClusterID(c.GetId()) != nil {
		t.Errorf("id = %q, want a daemon-made minikube-<random>", c.GetId())
	}
	if c.GetNamespace() != "default" || len(c.GetKubeconfigPaths()) != 1 || c.GetKubeconfigPaths()[0] != f.kubeconfig {
		t.Errorf("defaults: namespace %q, paths %v (want default and %s)", c.GetNamespace(), c.GetKubeconfigPaths(), f.kubeconfig)
	}
	if c.GetClusterUid() != "" || c.GetServer() != "" || c.GetRevision() != 1 || c.GetCreatedAt() == nil || c.GetUpdatedAt() == nil {
		t.Errorf("daemon-owned fields = uid %q server %q revision %d created %v", c.GetClusterUid(), c.GetServer(), c.GetRevision(), c.GetCreatedAt())
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING {
		t.Errorf("a new connection is %v, want CHECKING until its first probe", st.GetState())
	}
	if stored := f.stored(c.GetId()); stored.Context != "minikube" || stored.Revision != 1 {
		t.Errorf("stored = %+v", stored)
	}
	// The context's own namespace is the default when it names one.
	if st := f.add(&fleetgrpc.KubeCluster{Context: "prod"}); st.GetCluster().GetNamespace() != "team" {
		t.Errorf("prod namespace = %q, want the context's team", st.GetCluster().GetNamespace())
	}
	info, err := os.Stat(state.KubeClustersPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kube_clusters.json: %v %v", info, err)
	}
}

func TestAddKubeClusterValidation(t *testing.T) {
	f := newKubeFixture(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		c    *fleetgrpc.KubeCluster
		code codes.Code
		msg  string
	}{
		{"no context", &fleetgrpc.KubeCluster{}, codes.InvalidArgument, "context is required"},
		{"relative path", &fleetgrpc.KubeCluster{Context: "minikube", KubeconfigPaths: []string{"kube/config"}}, codes.InvalidArgument, "not an absolute path"},
		{"tilde path", &fleetgrpc.KubeCluster{Context: "minikube", KubeconfigPaths: []string{"~/.kube/config"}}, codes.InvalidArgument, "'~' is not expanded"},
		{"unknown context", &fleetgrpc.KubeCluster{Context: "nope"}, codes.NotFound, `no context "nope"`},
		{"bad namespace", &fleetgrpc.KubeCluster{Context: "minikube", Namespace: "Not_A_NS"}, codes.InvalidArgument, "namespace"},
		{"bad quantity", &fleetgrpc.KubeCluster{Context: "minikube", DiskSize: "fifty"}, codes.InvalidArgument, "disk_size"},
		{"zero quantity", &fleetgrpc.KubeCluster{Context: "minikube", MemoryLimit: "0"}, codes.InvalidArgument, "memory_limit"},
		{"bad enum", &fleetgrpc.KubeCluster{Context: "minikube", CloneMethod: "teleport"}, codes.InvalidArgument, "clone_method"},
		{"bad image mode", &fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Mode: "node_load"}}, codes.InvalidArgument, "unknown image delivery mode"},
		{"secret plugin env value", &fleetgrpc.KubeCluster{Context: "minikube", PluginEnv: []string{"AWS_SECRET_ACCESS_KEY=hunter2"}}, codes.InvalidArgument, "plugin_env"},
		{"relative extra path", &fleetgrpc.KubeCluster{Context: "minikube", ExtraPath: []string{"bin"}}, codes.InvalidArgument, "extra_path"},
		{"bad toleration", &fleetgrpc.KubeCluster{Context: "minikube", Tolerations: []*fleetgrpc.KubeToleration{{Key: "k", Operator: "Exists", Value: "v"}}}, codes.InvalidArgument, "tolerations[0]"},
		{"toleration seconds without NoExecute", &fleetgrpc.KubeCluster{Context: "minikube", Tolerations: []*fleetgrpc.KubeToleration{{Key: "k", TolerationSeconds: ptr(int64(5))}}}, codes.InvalidArgument, "NoExecute"},
		{"bad node selector", &fleetgrpc.KubeCluster{Context: "minikube", NodeSelector: map[string]string{"bad key!": "v"}}, codes.InvalidArgument, "node_selector"},
		{"overrides not a mapping", &fleetgrpc.KubeCluster{Context: "minikube", PodOverridesYaml: "- a\n- b\n"}, codes.InvalidArgument, "pod_overrides_yaml"},
		{"bad platform", &fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Platform: "arm64"}}, codes.InvalidArgument, "image.platform"},
		{"bad retention", &fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Retention: "forever"}}, codes.InvalidArgument, "image retention"},
		{"bad registry", &fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Registry: "https://reg.example/a b"}}, codes.InvalidArgument, "image:"},
		{"pace over 100", &fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{UploadPacePercent: ptr(int32(101))}}, codes.InvalidArgument, "upload pace"},
		{"bad fire interval", &fleetgrpc.KubeCluster{Context: "minikube", AutomationFireInterval: "often"}, codes.InvalidArgument, "automation_fire_interval"},
		{"negative max instances", &fleetgrpc.KubeCluster{Context: "minikube", MaxInstances: -1}, codes.InvalidArgument, "max_instances"},
		{"bad runtime class", &fleetgrpc.KubeCluster{Context: "minikube", RuntimeClass: "Sysbox Runc"}, codes.InvalidArgument, "runtime_class"},
		{"bad dind address pool", &fleetgrpc.KubeCluster{Context: "minikube", DindAddressPool: "base=10.200.0.0/16,size=8"}, codes.InvalidArgument, "dind_address_pool"},
		{"missing kubeconfig", &fleetgrpc.KubeCluster{Context: "minikube", KubeconfigPaths: []string{filepath.Join(dir, "none")}}, codes.NotFound, "no context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: tc.c})
			if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("AddKubeCluster = %v, want %v containing %q", err, tc.code, tc.msg)
			}
		})
	}
	if reg, _ := state.LoadKubeClusters(); len(reg.Clusters) != 0 {
		t.Fatalf("refused adds saved %+v", reg.Clusters)
	}
}

// Names are unique, and so is (kubeconfig paths, context, namespace); two
// contexts to one cluster may share a namespace.
func TestAddKubeClusterUniqueness(t *testing.T) {
	f := newKubeFixture(t)
	a := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "mk", Namespace: "fleet"})
	for _, tc := range []struct {
		c   *fleetgrpc.KubeCluster
		msg string
	}{
		{&fleetgrpc.KubeCluster{Context: "prod", DisplayName: "MK"}, `already named "MK"`},
		{&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}, "already uses context"},
	} {
		if _, err := f.svc.AddKubeCluster(context.Background(), &fleetgrpc.AddKubeClusterRequest{Cluster: tc.c}); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("AddKubeCluster(%v) = %v, want AlreadyExists %q", tc.c, err, tc.msg)
		}
	}
	b := f.add(&fleetgrpc.KubeCluster{Context: "minikube-alias", Namespace: "fleet"})
	if b.GetCluster().GetNamespace() != a.GetCluster().GetNamespace() || b.GetCluster().GetId() == a.GetCluster().GetId() {
		t.Errorf("two records on one namespace: %v and %v", a.GetCluster(), b.GetCluster())
	}
}

func TestUpdateKubeClusterFieldMask(t *testing.T) {
	f := newKubeFixture(t)
	st := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Image: &fleetgrpc.KubeImageSettings{Mode: "registry", Registry: "reg.example/a"}})
	id := st.GetCluster().GetId()
	// The gateway credential lives only in the file; no update may lose it.
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Find(id).Image.GatewayCredential = "gw-cred-SECRET"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	update := func(c *fleetgrpc.KubeCluster, rev int64, paths ...string) (*fleetgrpc.KubeClusterStatus, error) {
		reply, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{Id: id, Cluster: c, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, ExpectedRevision: rev})
		return reply.GetCluster(), err
	}

	got, err := update(&fleetgrpc.KubeCluster{AllowPrivileged: ptr(true), DiskSize: "999Gi"}, 1, "allow_privileged")
	if err != nil {
		t.Fatal(err)
	}
	c := got.GetCluster()
	if !c.GetAllowPrivileged() || c.GetDiskSize() != "" || c.GetRevision() != 2 || c.GetImage().GetRegistry() != "reg.example/a" {
		t.Errorf("after allow_privileged: %v", c)
	}
	// A sub-field path leaves the rest of the message alone.
	if got, err = update(&fleetgrpc.KubeCluster{Image: &fleetgrpc.KubeImageSettings{Mode: "gateway"}}, 0, "image.mode"); err != nil || got.GetCluster().GetImage().GetRegistry() != "reg.example/a" || got.GetCluster().GetImage().GetMode() != "gateway" {
		t.Errorf("image.mode: %v, %v", got.GetCluster().GetImage(), err)
	}
	// A message path replaces it whole; a path to an unset value clears it.
	if got, err = update(&fleetgrpc.KubeCluster{Image: &fleetgrpc.KubeImageSettings{Builder: "b"}}, 0, "image", "allow_privileged"); err != nil {
		t.Fatal(err)
	}
	if img := got.GetCluster().GetImage(); img.GetRegistry() != "" || img.GetBuilder() != "b" || got.GetCluster().AllowPrivileged != nil {
		t.Errorf("image replaced: %v, allow_privileged %v", img, got.GetCluster().AllowPrivileged)
	}
	if cred := f.stored(id).Image.GatewayCredential; cred != "gw-cred-SECRET" {
		t.Errorf("gateway credential after updates = %q", cred)
	}

	for _, tc := range []struct {
		paths []string
		rev   int64
		code  codes.Code
	}{
		{nil, 0, codes.InvalidArgument},
		{[]string{"cluster_uid"}, 0, codes.InvalidArgument},
		{[]string{"revision"}, 0, codes.InvalidArgument},
		{[]string{"no_such_field"}, 0, codes.InvalidArgument},
		{[]string{"display_name.x"}, 0, codes.InvalidArgument},
		{[]string{"display_name"}, 1, codes.Aborted},
	} {
		if _, err := update(&fleetgrpc.KubeCluster{DisplayName: "x"}, tc.rev, tc.paths...); status.Code(err) != tc.code {
			t.Errorf("update %v (rev %d) = %v, want %v", tc.paths, tc.rev, err, tc.code)
		}
	}
	if _, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{Id: "gone", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("update of a missing connection = %v", err)
	}
}

// Every settable field takes the value an update sends, and keeps it.
func TestUpdateKubeClusterEveryField(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	want := everyKubeField(t.TempDir())
	want.KubeconfigPaths = []string{f.kubeconfig}
	reply, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{Id: id, Cluster: want, UpdateMask: &fieldmaskpb.FieldMask{Paths: settableKubePaths()}})
	if err != nil {
		t.Fatalf("UpdateKubeCluster(every field): %v", err)
	}
	if got := clientView(reply.GetCluster().GetCluster()); !proto.Equal(got, want) {
		t.Errorf("stored record\n got %v\nwant %v", got, want)
	}
	if got := clientView(protoconv.KubeClusterToProto(f.stored(id))); !proto.Equal(got, want) {
		t.Errorf("kube_clusters.json\n got %v\nwant %v", got, want)
	}
}

// clientView is a record without its daemon-owned fields.
func clientView(c *fleetgrpc.KubeCluster) *fleetgrpc.KubeCluster {
	c = proto.Clone(c).(*fleetgrpc.KubeCluster)
	m := c.ProtoReflect()
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if daemonOwnedPaths[string(fd.Name())] {
			m.Clear(fd)
		}
		return true
	})
	return c
}

// A connection in use moves to another context only when that reaches the
// same cluster and sees its instances; its namespace cannot change. An unused
// one moves freely and records the new cluster afresh.
func TestUpdateKubeClusterRepoint(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	repoint := func(ctxName, ns string) error {
		_, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{
			Id: id, Cluster: &fleetgrpc.KubeCluster{Context: ctxName, Namespace: ns}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"context", "namespace"}},
		})
		return err
	}
	// Unused: free to move; the probe's notes are dropped.
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Find(id).ClusterUID = fakeClusterUID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repoint("not-in-the-kubeconfig", "team"); status.Code(err) != codes.NotFound {
		t.Errorf("re-point to a context the kubeconfig lacks = %v", err)
	}
	if err := repoint("prod", "team"); err != nil {
		t.Fatalf("re-point of an unused connection: %v", err)
	}
	if rec := f.stored(id); rec.ClusterUID != "" || rec.Context != "prod" {
		t.Errorf("after an unused re-point: %+v", rec)
	}
	if err := repoint("minikube", "fleet"); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Find(id).ClusterUID = fakeClusterUID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// In use by an instance whose StatefulSet lives in the cluster.
	key := names.InstanceKey(testIdentity.DaemonID, "alpha", "one", time.Unix(1700000000, 0))
	seedKubeInstance(t, "alpha", "one", id, key)
	if err := repoint("prod", "fleet"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "different cluster") {
		t.Errorf("re-point to another cluster = %v", err)
	}
	if err := repoint("minikube-alias", "fleet"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not visible") {
		t.Errorf("re-point that cannot see the instance = %v", err)
	}
	labels := names.Base(testIdentity.Owner(), id)
	labels[names.LabelRole] = string(names.RoleInstance)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: names.Instance("alpha", "one", key), Namespace: "fleet", Labels: labels}}
	if _, err := f.apis["minikube"].AppsV1().StatefulSets("fleet").Create(context.Background(), sts, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := repoint("minikube-alias", "team"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "namespace cannot change") {
		t.Errorf("namespace change of a used connection = %v", err)
	}
	if err := repoint("minikube-alias", "fleet"); err != nil {
		t.Fatalf("re-point to the same cluster: %v", err)
	}
	if rec := f.stored(id); rec.Context != "minikube-alias" || rec.ClusterUID != fakeClusterUID {
		t.Errorf("after the re-point: %+v", rec)
	}

	// No recorded cluster id: nothing proves the new target is the same.
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Find(id).ClusterUID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.kube.sync()
	if err := repoint("minikube", "fleet"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "never read") {
		t.Errorf("re-point without a cluster id = %v", err)
	}
}

func TestRemoveKubeCluster(t *testing.T) {
	f := newKubeFixture(t)
	unused := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	reply, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: unused})
	if err != nil || !reply.GetRemoved() {
		t.Fatalf("remove unused = %v, %v", reply, err)
	}

	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	key := names.InstanceKey(testIdentity.DaemonID, "alpha", "one", time.Unix(1700000000, 0))
	seedKubeInstance(t, "alpha", "one", id, key)
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "nightly", Backend: fleet.BackendKubernetes, KubeCluster: id}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.rememberDefaultTarget(fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: id}); err != nil {
		t.Fatal(err)
	}

	reply, err = f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id})
	if err != nil || reply.GetRemoved() || len(reply.GetUsers()) != 2 {
		t.Fatalf("remove in use = %v, %v; want refused with 2 users", reply, err)
	}
	if _, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id, Mode: fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "agent alpha/nightly") {
		t.Errorf("destroy-instances with an agent = %v", err)
	}

	// FORGET: the instance's StatefulSet is scaled to 0 and its record fails.
	labels := names.Base(testIdentity.Owner(), id)
	labels[names.LabelRole] = string(names.RoleInstance)
	one := int32(1)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: names.Instance("alpha", "one", key), Namespace: "fleet", Labels: labels}, Spec: appsv1.StatefulSetSpec{Replicas: &one}}
	if _, err := f.apis["minikube"].AppsV1().StatefulSets("fleet").Create(context.Background(), sts, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reply, err = f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id, Mode: fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET})
	if err != nil || !reply.GetRemoved() {
		t.Fatalf("forget = %v, %v", reply, err)
	}
	scaled, err := f.apis["minikube"].AppsV1().StatefulSets("fleet").Get(context.Background(), sts.Name, metav1.GetOptions{})
	if err != nil || scaled.Spec.Replicas == nil || *scaled.Spec.Replicas != 0 {
		t.Errorf("forget left the StatefulSet at %v (%v)", scaled.Spec.Replicas, err)
	}
	st, _ := state.Load()
	inst, _ := st.Fleets["alpha"].GetInstance("one")
	if inst.Status != fleet.StatusFailed || !strings.HasPrefix(inst.Error, "kubernetes:cluster_missing") {
		t.Errorf("forgotten instance = %v %q", inst.Status, inst.Error)
	}
	if cfg, _ := state.LoadConfig(); cfg.DefaultKubeCluster != "" || cfg.DefaultBackend != "" {
		t.Errorf("the remembered default still names the removed connection: %q %q", cfg.DefaultBackend, cfg.DefaultKubeCluster)
	}
	if reg, _ := state.LoadKubeClusters(); len(reg.Clusters) != 0 {
		t.Errorf("records after removal: %+v", reg.Clusters)
	}
}

// A create that lands between a removal's look at the users and the
// removal itself is still seen: the connection stays, and the reply names
// it.
func TestRemoveKubeClusterSeesACreateThatLandsMeanwhile(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	orig := beforeKubeRemove
	beforeKubeRemove = func() {
		seedKubeInstance(t, "alpha", "late", id, names.InstanceKey(testIdentity.DaemonID, "alpha", "late", time.Unix(1700000000, 0)))
	}
	t.Cleanup(func() { beforeKubeRemove = orig })
	reply, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id})
	if err != nil || reply.GetRemoved() || len(reply.GetUsers()) != 1 || reply.GetUsers()[0].GetInstance() != "late" {
		t.Fatalf("remove with a create landing meanwhile = %v, %v", reply, err)
	}
	if _, ok := f.svc.kube.record(id); !ok {
		t.Fatal("the connection was removed under an instance")
	}
}

// DESTROY_INSTANCES destroys the connection's instances, then removes it.
func TestRemoveKubeClusterDestroysInstances(t *testing.T) {
	f := newKubeFixture(t)
	startHub(t, f.svc)
	origDown := jobDownInstance
	jobDownInstance = func(*fleet.Instance) error { return nil }
	t.Cleanup(func() { jobDownInstance = origDown })
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	seedKubeInstance(t, "alpha", "one", id, "")
	seedKubeInstance(t, "alpha", "two", "elsewhere", "")
	reply, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id, Mode: fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES})
	if err != nil || !reply.GetRemoved() {
		t.Fatalf("destroy-instances = %v, %v", reply, err)
	}
	st, _ := state.Load()
	if _, err := st.Fleets["alpha"].GetInstance("one"); err == nil {
		t.Error("the connection's instance survived")
	}
	if _, err := st.Fleets["alpha"].GetInstance("two"); err != nil {
		t.Error("another connection's instance was destroyed")
	}
}

// A test streams every check, and its result becomes the connection's
// health; an unsaved draft is judged without saving or claiming anything.
func TestTestKubeCluster(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	client := dialBuf(t, newLocalServer(f.svc))
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()

	events := collectTest(t, client, &fleetgrpc.TestKubeClusterRequest{Id: id})
	result := events[len(events)-1].GetResult()
	if result.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Fatalf("test result = %v %q; checks %v", result.GetState(), result.GetMessage(), result.GetChecks())
	}
	var running, done int
	for _, ev := range events[:len(events)-1] {
		switch ev.GetCheck().GetResult() {
		case fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING:
			running++
		default:
			done++
		}
	}
	if running == 0 || done < len(result.GetChecks()) {
		t.Errorf("streamed %d running and %d finished checks for %d results", running, done, len(result.GetChecks()))
	}
	if st := f.svc.kube.status(id); st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Errorf("the test did not become the health: %v", st.GetState())
	}
	if rec := f.stored(id); rec.ClusterUID != fakeClusterUID || rec.Server != "https://minikube.test:8443" || rec.VerifiedAt.IsZero() || rec.Revision != 1 {
		t.Errorf("bookkeeping after the test: %+v", rec)
	}
	if _, err := f.apis["minikube"].CoordinationV1().Leases("fleet").Get(context.Background(), names.OwnerLease(testIdentity.DaemonID, id), metav1.GetOptions{}); err != nil {
		t.Errorf("the saved connection's owner Lease: %v", err)
	}

	events = collectTest(t, client, &fleetgrpc.TestKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "prod"}})
	if r := events[len(events)-1].GetResult(); r.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED || r.GetCluster().GetNamespace() != "team" {
		t.Errorf("draft result = %v %q in %q", r.GetState(), r.GetMessage(), r.GetCluster().GetNamespace())
	}
	if leases, _ := f.apis["prod"].CoordinationV1().Leases("team").List(context.Background(), metav1.ListOptions{}); len(leases.Items) != 0 {
		t.Errorf("an unsaved draft claimed a Lease: %v", leases.Items)
	}
	if reg, _ := state.LoadKubeClusters(); len(reg.Clusters) != 1 {
		t.Errorf("the draft was saved: %+v", reg.Clusters)
	}
}

func collectTest(t *testing.T, client fleetgrpc.FleetServiceClient, req *fleetgrpc.TestKubeClusterRequest) []*fleetgrpc.TestKubeClusterEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := client.TestKubeCluster(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var events []*fleetgrpc.TestKubeClusterEvent
	for {
		ev, err := stream.Recv()
		if err != nil {
			if len(events) == 0 || events[len(events)-1].GetResult() == nil {
				t.Fatalf("TestKubeCluster ended with %v before its result", err)
			}
			return events
		}
		events = append(events, ev)
	}
}

// Workers probe in the background and push health over Watch: an empty set
// in the initial snapshot, then each connection CHECKING, then READY.
func TestKubeHealthIsPushedOverWatch(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	client := dialBuf(t, newLocalServer(f.svc))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatal(err)
	}
	next := func() *fleetgrpc.KubeClusters {
		for {
			ev, err := stream.Recv()
			if err != nil {
				t.Fatalf("watch: %v", err)
			}
			if kc := ev.GetKubeClusters(); kc != nil {
				return kc
			}
		}
	}
	if initial := next(); len(initial.GetClusters()) != 0 {
		t.Fatalf("initial kube clusters = %v", initial)
	}
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	for {
		kc := next()
		if len(kc.GetClusters()) != 1 || kc.GetClusters()[0].GetCluster().GetId() != id {
			t.Fatalf("pushed %v", kc)
		}
		if kc.GetClusters()[0].GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
			if caps := kc.GetClusters()[0].GetCapabilities(); caps.GetVersion() != "v1.37.0" || !caps.GetLocalCluster() {
				t.Errorf("pushed capabilities = %v", caps)
			}
			return
		}
	}
}

// A restored copy of the daemon id on another machine (another incarnation)
// finds the connection held: OWNER_CONFLICT, and it refuses to prune.
func TestKubeOwnerConflictFromARestoredDaemonID(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet"}).GetCluster().GetId()
	if st, err := f.svc.kube.test(context.Background(), id, false, nil); err != nil || st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY {
		t.Fatalf("original daemon: %v %v", st.GetState(), err)
	}

	restored := newService()
	restored.kube.environ, restored.kube.connect = f.svc.kube.environ, f.svc.kube.connect
	other := testIdentity
	other.Nonce, other.Host = "bbbbbbbbbbbbbbbb", "desktop"
	restored.kube.loadIdentity = func() (owner.Identity, error) { return other, nil }
	st, err := restored.kube.test(context.Background(), id, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT || !strings.Contains(st.GetMessage(), "on laptop") {
		t.Fatalf("restored daemon = %v %q, want OWNER_CONFLICT naming laptop", st.GetState(), st.GetMessage())
	}
	if _, err := restored.PruneKubeCluster(context.Background(), &fleetgrpc.PruneKubeClusterRequest{Id: id, Apply: true}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "owner_conflict") {
		t.Errorf("prune --apply from the restored daemon = %v", err)
	}
	dry, err := restored.PruneKubeCluster(context.Background(), &fleetgrpc.PruneKubeClusterRequest{Id: id})
	if err != nil || dry.GetLeaseHeld() || dry.GetLeaseHolderHost() != "laptop" {
		t.Errorf("dry-run prune from the restored daemon = %v, %v", dry, err)
	}
}

func TestListKubeContexts(t *testing.T) {
	f := newKubeFixture(t)
	stubDaemonHost(t, "fleetd-host")
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	bin := t.TempDir()
	writePluginScript(t, filepath.Join(bin, "found-plugin"), "exit 0")
	f.svc.kube.environ = func() []string { return []string{"PATH=" + bin, "KUBECONFIG=" + f.kubeconfig} }
	extra := writeKubeconfigFile(t, filepath.Join(t.TempDir(), "other"), "", `
- name: exec-found
  context: {cluster: minikube, user: found}
- name: exec-missing
  context: {cluster: minikube, user: missing}
`, `
- name: found
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: found-plugin}}
- name: missing
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: aws}}
`)

	reply, err := f.svc.ListKubeContexts(context.Background(), &fleetgrpc.ListKubeContextsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetHost() != "fleetd-host" || len(reply.GetKubeconfigPaths()) != 1 || reply.GetKubeconfigPaths()[0] != f.kubeconfig {
		t.Errorf("reply host %q paths %v", reply.GetHost(), reply.GetKubeconfigPaths())
	}
	byName := map[string]*fleetgrpc.KubeContext{}
	for _, c := range reply.GetContexts() {
		byName[c.GetName()] = c
	}
	if c := byName["minikube"]; c == nil || !c.GetCurrent() || len(c.GetConnected()) != 1 || c.GetConnected()[0] != id || c.GetAuth() != "client-cert" {
		t.Errorf("minikube = %v", c)
	}
	if c := byName["token-ctx"]; c == nil || c.GetAuth() != "token" || c.GetConnected() != nil {
		t.Errorf("token-ctx = %v", c)
	}

	// Any kubeconfig on the daemon host, as a remote picker's ctrl+o asks.
	reply, err = f.svc.ListKubeContexts(context.Background(), &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: []string{extra}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range reply.GetContexts() {
		switch c.GetName() {
		case "exec-found":
			if c.GetPluginPath() != filepath.Join(bin, "found-plugin") || c.GetPluginMissing() != "" {
				t.Errorf("exec-found = %v", c)
			}
		case "exec-missing":
			if c.GetPluginPath() != "" || !strings.Contains(c.GetPluginMissing(), "not on the daemon host's PATH") {
				t.Errorf("exec-missing = %v", c)
			}
		}
	}
	if _, err := f.svc.ListKubeContexts(context.Background(), &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: []string{"relative"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("relative path = %v", err)
	}
}

func writePluginScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCreateKubeNamespace(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", Namespace: "fleet-new"}).GetCluster().GetId()
	dry, err := f.svc.CreateKubeNamespace(context.Background(), &fleetgrpc.CreateKubeNamespaceRequest{Id: id, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  labels:\n    app.kubernetes.io/managed-by: fleet\n  name: fleet-new\n"
	if dry.GetManifest() != want || dry.GetCreated() {
		t.Errorf("dry run = %q (created %v), want\n%s", dry.GetManifest(), dry.GetCreated(), want)
	}
	if _, err := f.apis["minikube"].CoreV1().Namespaces().Get(context.Background(), "fleet-new", metav1.GetOptions{}); err == nil {
		t.Fatal("a dry run created the namespace")
	}
	reply, err := f.svc.CreateKubeNamespace(context.Background(), &fleetgrpc.CreateKubeNamespaceRequest{Id: id, Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}})
	if err != nil || !reply.GetCreated() {
		t.Fatalf("create = %v, %v", reply, err)
	}
	ns, err := f.apis["minikube"].CoreV1().Namespaces().Get(context.Background(), "fleet-new", metav1.GetOptions{})
	if err != nil || ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Errorf("created namespace = %v, %v", ns, err)
	}
	if _, err := f.svc.CreateKubeNamespace(context.Background(), &fleetgrpc.CreateKubeNamespaceRequest{Id: id}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second create = %v", err)
	}
	if _, err := f.svc.CreateKubeNamespace(context.Background(), &fleetgrpc.CreateKubeNamespaceRequest{Cluster: &fleetgrpc.KubeCluster{Context: "minikube"}, Namespace: "Bad_Name"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad name = %v", err)
	}
}

// Cluster deploy targets are one per connection, available only when it is
// healthy and fleetd has docker and the devcontainer CLI.
func TestKubeDeployTargets(t *testing.T) {
	f := newKubeFixture(t)
	stubDaemonHost(t, "h", "devcontainer", "docker")
	f.start()
	ready := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "mk"}).GetCluster().GetId()
	broken := f.add(&fleetgrpc.KubeCluster{Context: "token-ctx"}).GetCluster().GetId()
	f.awaitState(ready, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)
	f.awaitState(broken, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING)

	clusterTargets := func() []fleet.DeployTargetOption {
		var out []fleet.DeployTargetOption
		for _, o := range f.svc.deployTargetOptions("git@example.com:a.git") {
			if o.Target.Backend == fleet.BackendKubernetes {
				out = append(out, o)
			}
		}
		return out
	}
	got := clusterTargets()
	if len(got) != 2 || got[0].Target.KubeCluster != ready || got[1].Target.KubeCluster != broken {
		t.Fatalf("cluster targets = %+v", got)
	}
	if !got[0].Available || got[1].Available || !strings.HasPrefix(got[1].Reason, "cluster context missing: ") {
		t.Errorf("targets = %+v", got)
	}
	stubDaemonHost(t, "h", "devcontainer")
	if got = clusterTargets(); got[0].Available || got[0].Reason != "docker not found where fleetd runs" {
		t.Errorf("without docker: %+v", got[0])
	}
	// Template fleets copy their template on the daemon host, which the
	// seed takes into the pod: every cluster is listed for them too, and
	// only Coder and Codespaces are dropped.
	var templateClusters []string
	for _, o := range f.svc.deployTargetOptions("file:///srv/template") {
		switch o.Target.Backend {
		case fleet.BackendKubernetes:
			templateClusters = append(templateClusters, o.Target.KubeCluster)
		case fleet.BackendCoder, fleet.BackendCodespaces:
			t.Errorf("a template fleet lists %+v", o)
		}
	}
	if len(templateClusters) != 2 || templateClusters[0] != ready || templateClusters[1] != broken {
		t.Errorf("a template fleet's cluster targets = %v, want [%s %s]", templateClusters, ready, broken)
	}
	// Over the wire too.
	reply, err := f.svc.ListDeployTargets(context.Background(), &fleetgrpc.ListDeployTargetsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var wire int
	for _, o := range reply.GetTargets() {
		if o.GetTarget().GetBackend() == fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES {
			wire++
		}
	}
	if wire != 2 {
		t.Errorf("ListDeployTargets carries %d cluster targets, want 2", wire)
	}
}

// A Kubernetes create that names no cluster goes to the remembered one, else
// the only one.
func TestResolveTargetPicksTheCluster(t *testing.T) {
	f := newKubeFixture(t)
	req := &fleetgrpc.CreateInstanceRequest{Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES}
	if got := f.svc.resolveTarget(req, ""); got.KubeCluster != "" {
		t.Errorf("no connections: %+v", got)
	}
	a := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	if got := f.svc.resolveTarget(req, ""); got.KubeCluster != a {
		t.Errorf("one connection: %+v", got)
	}
	b := f.add(&fleetgrpc.KubeCluster{Context: "prod"}).GetCluster().GetId()
	if got := f.svc.resolveTarget(req, ""); got.KubeCluster != "" {
		t.Errorf("two connections, none remembered: %+v", got)
	}
	if err := f.svc.rememberDefaultTarget(fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: b}); err != nil {
		t.Fatal(err)
	}
	if got := f.svc.resolveTarget(req, ""); got.KubeCluster != b {
		t.Errorf("remembered %s: %+v", b, got)
	}
	explicit := &fleetgrpc.CreateInstanceRequest{Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES, KubeCluster: a}
	if got := f.svc.resolveTarget(explicit, ""); got.KubeCluster != a {
		t.Errorf("explicit: %+v", got)
	}
}

// An unreadable state.json refuses a removal rather than guessing that no
// instance uses the connection.
func TestRemoveKubeClusterRefusesWithoutState(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	if err := os.WriteFile(state.StatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id}); status.Code(err) != codes.Internal {
		t.Fatalf("remove with a broken state.json = %v", err)
	}
	if reg, _ := state.LoadKubeClusters(); len(reg.Clusters) != 1 {
		t.Fatal("the connection was removed")
	}
}

// A Kubernetes create needs an existing connection with room under its
// quotas.
func TestKubeCreateAllowed(t *testing.T) {
	f := newKubeFixture(t)
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", MaxInstances: 2, MaxTotalDisk: "120Gi", DiskSize: "50Gi"}).GetCluster().GetId()
	target := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: id}
	st := &state.State{Fleets: map[string]*fleet.Fleet{}}
	if err := f.svc.kube.createAllowed(st, target); err != nil {
		t.Fatalf("an empty connection: %v", err)
	}
	if err := f.svc.kube.createAllowed(st, fleet.DeployTarget{Backend: fleet.BackendDevcontainer}); err != nil {
		t.Fatalf("devcontainer: %v", err)
	}
	for _, tc := range []struct {
		target fleet.DeployTarget
		code   codes.Code
	}{
		{fleet.DeployTarget{Backend: fleet.BackendKubernetes}, codes.InvalidArgument},
		{fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "gone-aaaaaa"}, codes.NotFound},
	} {
		if err := f.svc.kube.createAllowed(st, tc.target); status.Code(err) != tc.code {
			t.Errorf("%+v = %v, want %v", tc.target, err, tc.code)
		}
	}
	st.Fleets["alpha"] = &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{
		{Name: "one", KubeCluster: id, KubeKey: "aaaaaaaa", Status: fleet.StatusRunning},
		{Name: "leaving", KubeCluster: id, KubeKey: "bbbbbbbb", Status: fleet.StatusDeleting},
		// A failed automation run whose pod and disk were released holds
		// no key, and nothing in the cluster.
		{Name: "released", KubeCluster: id, Status: fleet.StatusFailed, Automated: true},
	}}
	// One instance: a second 50Gi disk makes 100Gi, under 120Gi.
	if err := f.svc.kube.createAllowed(st, target); err != nil {
		t.Errorf("one used: %v", err)
	}
	// One more about to be created (an automation run's reservation) fills
	// max_instances.
	if err := f.svc.kube.quotaRoom(st, id, 1); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "allows 2 instances and has 2") {
		t.Errorf("one used and one reserved = %v", err)
	}
	st.Fleets["alpha"].Instances = append(st.Fleets["alpha"].Instances, &fleet.Instance{Name: "two", KubeCluster: id, KubeKey: "cccccccc", Status: fleet.StatusFailed})
	if err := f.svc.kube.createAllowed(st, target); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "allows 2 instances") {
		t.Errorf("at max_instances = %v", err)
	}
	if _, err := f.svc.UpdateKubeCluster(context.Background(), &fleetgrpc.UpdateKubeClusterRequest{Id: id, Cluster: &fleetgrpc.KubeCluster{}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_instances"}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.kube.createAllowed(st, target); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "of instance disks") {
		t.Errorf("over max_total_disk = %v", err)
	}
}

// The daemon's pool learns a connection the moment it is added (and forgets
// it when it is removed), so a create on a connection added a moment ago
// finds it rather than waiting for the pool's own re-read.
func TestKubeConnectionsReachThePoolAtOnce(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube"}).GetCluster().GetId()
	if got := f.pooledIDs(); !slices.Contains(got, id) {
		t.Fatalf("after the add the pool has %v, want %s", got, id)
	}
	if _, err := f.svc.RemoveKubeCluster(context.Background(), &fleetgrpc.RemoveKubeClusterRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	if got := f.pooledIDs(); slices.Contains(got, id) {
		t.Fatalf("after the remove the pool still has %v", got)
	}
}
