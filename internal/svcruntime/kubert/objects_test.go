//go:build !nok8s

package kubert

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// The golden files under testdata are rewritten instead of checked with
// FLEET_KUBERT_UPDATE=1; review the diff before committing it.
func updating() bool { return os.Getenv("FLEET_KUBERT_UPDATE") == "1" }

func checkGolden(t *testing.T, name string, objs []metav1.Object) {
	t.Helper()
	var b bytes.Buffer
	for _, o := range objs {
		body, err := podspec.ApplyBody(o)
		if err != nil {
			t.Fatal(err)
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err != nil {
			t.Fatal(err)
		}
		b.Write(pretty.Bytes())
		b.WriteString("\n---\n")
	}
	path := filepath.Join("testdata", name)
	if updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (record it with FLEET_KUBERT_UPDATE=1)", err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("%s differs from what the builders render now; if intended, record it with FLEET_KUBERT_UPDATE=1 and review the diff\ngot:\n%s", path, b.String())
	}
}

// testMeta is a fleet on a connection, as the builders' tests name it.
var testMeta = fleetMeta{Namespace: "fleet", Owner: "0123456789ab", Cluster: "minikube-abc234", Fleet: "fleet-man", Hash: names.FleetHash("0123456789abcdef", "minikube-abc234", "fleet-man")}

var testAnchor = anchorRef(testMeta.name(names.SuffixAnchor), "11111111-2222-3333-4444-555555555555")

// fleetObjects renders every object of the fleet on rec, as a connection
// that runs each service would have them.
func fleetObjects(t *testing.T, rec *fleet.KubeCluster, rootful bool) []metav1.Object {
	t.Helper()
	objs := []metav1.Object{anchorConfigMap(testMeta), netService(testMeta, testAnchor), instancesPolicy(testMeta, testAnchor), cachesPolicy(testMeta, testAnchor), egressPolicy(testMeta, testAnchor)}
	for _, svc := range fleet.SharedServices() {
		size, err := cacheSize(rec, svc)
		if err != nil {
			t.Fatal(err)
		}
		objs = append(objs, cacheVolume(testMeta, rec, svc, size))
		if svc == fleet.ServiceBuildkit {
			objs = append(objs, buildkitConfig(testMeta, testAnchor))
		}
		dep, err := cacheDeployment(testMeta, svc, testAnchor, deploymentInput{Rec: rec, Rootful: rootful})
		if err != nil {
			t.Fatalf("%s: %v", svc, err)
		}
		objs = append(objs, dep, cacheService(testMeta, svc, testAnchor))
	}
	return objs
}

// TestFleetObjectsGolden pins every object a fleet gets on a connection with
// default settings, and on one with cache settings of its own and rootful
// buildkit.
func TestFleetObjectsGolden(t *testing.T) {
	checkGolden(t, "fleet-default.json", fleetObjects(t, &fleet.KubeCluster{}, false))
	tuned := &fleet.KubeCluster{
		CacheStorageClass: "fast-rwo",
		CacheSizes:        fleet.KubeCacheSizes{DebCache: "4Gi", ImageCache: "50Gi", Buildkit: "100Gi"},
		ServiceImages:     fleet.KubeServiceImages{DebCache: "registry.example.com/acng@sha256:" + strings.Repeat("a", 64)},
		NodeSelector:      map[string]string{"pool": "fleet"},
		Tolerations:       []fleet.KubeToleration{{Key: "dedicated", Operator: "Equal", Value: "fleet", Effect: "NoSchedule"}},
		ServiceAccount:    "fleet-pods",
		MemoryLimit:       "16Gi",
	}
	checkGolden(t, "fleet-tuned.json", fleetObjects(t, tuned, true))
}

// TestFleetObjectsAreLabelledAndOwned: every object carries the connection's
// labels, the fleet's h8 and its role; every one but the anchor and the cache
// volumes names the anchor as its owner; every name is a Service name.
func TestFleetObjectsAreLabelledAndOwned(t *testing.T) {
	for _, o := range fleetObjects(t, &fleet.KubeCluster{}, false) {
		l := o.GetLabels()
		for k, v := range names.Base(testMeta.Owner, testMeta.Cluster) {
			if l[k] != v {
				t.Errorf("%s: label %s=%q, want %q", o.GetName(), k, l[k], v)
			}
		}
		if l[names.LabelFleet] != testMeta.Hash || l[names.LabelRole] == "" {
			t.Errorf("%s: labels %v", o.GetName(), l)
		}
		if !names.IsDNS1035Label(o.GetName()) {
			t.Errorf("%q is not a Service name", o.GetName())
		}
		_, isPVC := o.(*corev1.PersistentVolumeClaim)
		isAnchor := o.GetName() == testMeta.name(names.SuffixAnchor)
		owned := len(o.GetOwnerReferences()) == 1 && o.GetOwnerReferences()[0].UID == testAnchor.UID
		if owned == (isPVC || isAnchor) {
			t.Errorf("%s: owners %v (cache volumes and the anchor have none, the rest the anchor)", o.GetName(), o.GetOwnerReferences())
		}
		if isPVC && l[names.LabelRole] != string(names.RoleCache) {
			t.Errorf("%s: a cache volume with role %q", o.GetName(), l[names.LabelRole])
		}
		if o.GetNamespace() != testMeta.Namespace {
			t.Errorf("%s: namespace %q", o.GetName(), o.GetNamespace())
		}
	}
}

// TestServicePodsAreHardened: every server is hardened, its image pinned,
// placed like the connection's instances.
func TestServicePodsAreHardened(t *testing.T) {
	rec := &fleet.KubeCluster{NodeSelector: map[string]string{"pool": "fleet"}}
	for _, rootful := range []bool{false, true} {
		for _, svc := range fleet.SharedServices() {
			dep, err := cacheDeployment(testMeta, svc, testAnchor, deploymentInput{Rec: rec, Rootful: rootful})
			if err != nil {
				t.Fatal(err)
			}
			tpl := dep.Spec.Template
			if v := podspec.CheckHardened(tpl.ObjectMeta, tpl.Spec, podspec.HardenOptions{PinnedImages: true}); len(v) > 0 {
				t.Errorf("%s: %v", svc, v)
			}
			if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *dep.Spec.Replicas != 1 {
				t.Errorf("%s: strategy %v replicas %d", svc, dep.Spec.Strategy.Type, *dep.Spec.Replicas)
			}
			if tpl.Spec.NodeSelector["pool"] != "fleet" {
				t.Errorf("%s: placement %v", svc, tpl.Spec.NodeSelector)
			}
			sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
			if err != nil || !sel.Matches(labels.Set(tpl.Labels)) {
				t.Errorf("%s: selector %v does not select its pods %v", svc, dep.Spec.Selector, tpl.Labels)
			}
			if c := tpl.Spec.Containers[0]; c.Ports[0].ContainerPort != cacheSpecs[svc].port {
				t.Errorf("%s: port %d", svc, c.Ports[0].ContainerPort)
			}
			if privileged := tpl.Spec.Containers[0].SecurityContext.Privileged; (privileged != nil && *privileged) != (rootful && svc == fleet.ServiceBuildkit) {
				t.Errorf("%s rootful=%v: privileged %v", svc, rootful, privileged)
			}
		}
	}
	// An override that is not pinned is refused, not run.
	rec.ServiceImages.ImageCache = "registry:2"
	if _, err := cacheDeployment(testMeta, fleet.ServiceImageCache, testAnchor, deploymentInput{Rec: rec}); err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Fatalf("an unpinned image: %v", err)
	}
}

// instancePodLabels are an instance pod's labels as the renderer gives them.
func instancePodLabels() labels.Set {
	l := names.Base(testMeta.Owner, testMeta.Cluster)
	l[names.LabelFleet], l[names.LabelKey], l[names.LabelRole], l[names.LabelShareGroup] = testMeta.Hash, "aaaaaaaa", string(names.RoleInstance), testMeta.Hash
	return l
}

func selects(t *testing.T, s metav1.LabelSelector, l labels.Set) bool {
	t.Helper()
	sel, err := metav1.LabelSelectorAsSelector(&s)
	if err != nil {
		t.Fatal(err)
	}
	return sel.Matches(l)
}

// TestFleetNetwork: the headless Service selects the fleet's instance pods
// (and is what their StatefulSets name); the instances policy covers the
// instances and their Jobs but not the caches and admits only the fleet's
// instances; the caches policy admits the fleet's instances on the
// services' ports; the egress policy covers every pod of the fleet and
// keeps it from the metadata ranges while allowing DNS anywhere.
func TestFleetNetwork(t *testing.T) {
	inst := instancePodLabels()
	seed := labels.Set{}
	for k, v := range inst {
		seed[k] = v
	}
	seed[names.LabelRole] = string(names.RoleSeed)
	cache := labels.Set(testMeta.serviceLabels(fleet.ServiceDebCache))
	other := labels.Set{names.LabelFleet: "zzzzzzzz", names.LabelRole: string(names.RoleInstance)}

	svc := netService(testMeta, testAnchor)
	if svc.Spec.ClusterIP != corev1.ClusterIPNone || !svc.Spec.PublishNotReadyAddresses {
		t.Errorf("net service %+v", svc.Spec)
	}
	if !labels.SelectorFromSet(svc.Spec.Selector).Matches(inst) || labels.SelectorFromSet(svc.Spec.Selector).Matches(seed) {
		t.Errorf("net service selector %v", svc.Spec.Selector)
	}
	if want := names.FleetObject(testMeta.Fleet, testMeta.Hash, names.SuffixNet); svc.Name != want {
		t.Errorf("net service %q, the StatefulSets name %q", svc.Name, want)
	}

	ip := instancesPolicy(testMeta, testAnchor)
	if !selects(t, ip.Spec.PodSelector, inst) || !selects(t, ip.Spec.PodSelector, seed) || selects(t, ip.Spec.PodSelector, cache) || selects(t, ip.Spec.PodSelector, other) {
		t.Errorf("instances policy selects %v", ip.Spec.PodSelector)
	}
	from := ip.Spec.Ingress[0].From[0].PodSelector
	if !selects(t, *from, inst) || selects(t, *from, other) || selects(t, *from, cache) || len(ip.Spec.Ingress[0].Ports) != 0 {
		t.Errorf("instances policy admits %v", ip.Spec.Ingress)
	}

	cp := cachesPolicy(testMeta, testAnchor)
	if !selects(t, cp.Spec.PodSelector, cache) || selects(t, cp.Spec.PodSelector, inst) {
		t.Errorf("caches policy selects %v", cp.Spec.PodSelector)
	}
	ports := map[int32]bool{}
	for _, p := range cp.Spec.Ingress[0].Ports {
		ports[p.Port.IntVal] = true
	}
	if !ports[3142] || !ports[5000] || !ports[1234] || len(ports) != 3 {
		t.Errorf("caches policy ports %v", ports)
	}
	if !selects(t, *cp.Spec.Ingress[0].From[0].PodSelector, inst) || selects(t, *cp.Spec.Ingress[0].From[0].PodSelector, other) {
		t.Errorf("caches policy admits %v", cp.Spec.Ingress[0].From)
	}

	ep := egressPolicy(testMeta, testAnchor)
	for _, l := range []labels.Set{inst, seed, cache} {
		if !selects(t, ep.Spec.PodSelector, l) {
			t.Errorf("egress policy does not cover %v", l)
		}
	}
	if ep.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(ep.Spec.Ingress) != 0 {
		t.Errorf("egress policy types %v", ep.Spec.PolicyTypes)
	}
	blocks := ep.Spec.Egress[0].To
	if blocks[0].IPBlock.CIDR != "0.0.0.0/0" || blocks[0].IPBlock.Except[0] != "169.254.0.0/16" ||
		blocks[1].IPBlock.CIDR != "::/0" || blocks[1].IPBlock.Except[0] != "fd00:ec2::/64" {
		t.Errorf("egress blocks %+v", blocks)
	}
	if ep.Spec.Egress[1].To[0].NamespaceSelector == nil || len(ep.Spec.Egress[1].To[0].NamespaceSelector.MatchLabels) != 0 {
		t.Errorf("egress to pods %+v", ep.Spec.Egress[1])
	}
	dns := ep.Spec.Egress[2]
	if len(dns.To) != 0 || len(dns.Ports) != 2 || dns.Ports[0].Port.IntVal != 53 || *dns.Ports[0].Protocol != corev1.ProtocolUDP || *dns.Ports[1].Protocol != corev1.ProtocolTCP {
		t.Errorf("egress DNS %+v", dns)
	}
}

// TestCacheCleanup: each cache cleans up below its volume. Buildkit's GC
// keeps the cache under 80% of the disk it is on; the registry mirror may
// delete what it proxied (its 7-day expiry); the deb cache's expiry run is
// apt-cacher-ng's own.
func TestCacheCleanup(t *testing.T) {
	cm := buildkitConfig(testMeta, testAnchor)
	if toml := cm.Data["buildkitd.toml"]; !strings.Contains(toml, "gc = true") || !strings.Contains(toml, `maxUsedSpace = "80%"`) {
		t.Errorf("buildkitd.toml:\n%s", toml)
	}
	for _, rootful := range []bool{false, true} {
		dep, _ := cacheDeployment(testMeta, fleet.ServiceBuildkit, testAnchor, deploymentInput{Rootful: rootful})
		c := dep.Spec.Template.Spec.Containers[0]
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == volumeConfig && strings.HasSuffix(c.Args[len(c.Args)-1], m.MountPath+"/buildkitd.toml") {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("rootful=%v: buildkitd does not read its config: args %v mounts %v", rootful, c.Args, c.VolumeMounts)
		}
	}
	dep, _ := cacheDeployment(testMeta, fleet.ServiceImageCache, testAnchor, deploymentInput{})
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["REGISTRY_PROXY_REMOTEURL"] != "https://registry-1.docker.io" || env["REGISTRY_STORAGE_DELETE_ENABLED"] != "true" {
		t.Errorf("registry env %v", env)
	}
	if !strings.Contains(strings.Join(aptMaintArgv, " "), "acngtool maint") {
		t.Errorf("apt expiry argv %v", aptMaintArgv)
	}
}

func TestCacheSizes(t *testing.T) {
	for _, tt := range []struct {
		rec  fleet.KubeCacheSizes
		svc  fleet.SharedService
		want string
	}{
		{svc: fleet.ServiceDebCache, want: "10Gi"},
		{svc: fleet.ServiceImageCache, want: "20Gi"},
		{svc: fleet.ServiceBuildkit, want: "20Gi"},
		{rec: fleet.KubeCacheSizes{DebCache: "3Gi"}, svc: fleet.ServiceDebCache, want: "3Gi"},
	} {
		q, err := cacheSize(&fleet.KubeCluster{CacheSizes: tt.rec}, tt.svc)
		if err != nil || q.String() != tt.want {
			t.Errorf("%s %+v: %v %v", tt.svc, tt.rec, q.String(), err)
		}
	}
	if _, err := cacheSize(&fleet.KubeCluster{CacheSizes: fleet.KubeCacheSizes{ImageCache: "lots"}}, fleet.ServiceImageCache); err == nil {
		t.Error("a bad size parsed")
	}
	for in, want := range map[string]string{"10Gi": "15Gi", "1Gi": "2Gi", "3Gi": "5Gi", "15Gi": "23Gi"} {
		if got := growQuantity(resource.MustParse(in)); got.String() != want {
			t.Errorf("grow %s = %s, want %s", in, got.String(), want)
		}
	}
}
