//go:build !nok8s

package kubert

import (
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// objects.go renders a fleet's objects on one connection. The builders are
// pure: the same fleet, connection record and anchor render the same objects,
// which the golden tests hold them to.

// The services' default images, pinned by digest (the connection's
// service_images overrides them; Harden refuses an override that is not
// pinned).
const (
	DefaultDebCacheImage         = "docker.io/sameersbn/apt-cacher-ng:latest@sha256:82f55f9c8f627cee8ef5f710c1745388d79d3a3f2f3150353e6500021bec11b4"
	DefaultImageCacheImage       = "docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	DefaultBuildkitRootlessImage = "docker.io/moby/buildkit:v0.25.1-rootless@sha256:0c0ce4929976ace509f7a53878303504b2de8857829aea4932af9f7325a30f51"
	DefaultBuildkitImage         = "docker.io/moby/buildkit:v0.25.1@sha256:79cc6476ab1a3371c9afd8b44e7c55610057c43e18d9b39b68e2b0c2475cc1b6"
)

// The metadata ranges the egress policy keeps a fleet's pods from: IPv4
// link-local (AWS, GCP and Azure serve instance metadata at 169.254.169.254)
// and AWS's IPv6 metadata range.
const (
	metadataRangeV4 = "169.254.0.0/16"
	metadataRangeV6 = "fd00:ec2::/64"
)

// gcKeepPercent is how much of its volume Buildkit's cache may use before
// its GC prunes it.
const gcKeepPercent = 80

// The services' upstream: the registry mirror proxies Docker Hub, the only
// registry dockerd's registry-mirrors setting applies to.
const imageCacheUpstream = "https://registry-1.docker.io"

// fleetMeta is what names and labels one fleet's objects on one connection.
type fleetMeta struct {
	Namespace string
	// Owner is the daemon's owner label (names.Owner), Cluster the
	// connection's record id.
	Owner, Cluster string
	// Fleet is the fleet's name, Hash its h8 on the connection.
	Fleet, Hash string
}

func (m fleetMeta) name(suffix string) string { return names.FleetObject(m.Fleet, m.Hash, suffix) }

// labels are the labels of the fleet's objects with role.
func (m fleetMeta) labels(role names.Role) map[string]string {
	l := names.Base(m.Owner, m.Cluster)
	l[names.LabelFleet] = m.Hash
	l[names.LabelRole] = string(role)
	return l
}

// serviceLabels are the labels of svc's objects.
func (m fleetMeta) serviceLabels(svc fleet.SharedService) map[string]string {
	l := m.labels(names.RoleCache)
	l[names.LabelService] = string(svc)
	return l
}

// fleetSelector selects every object of the fleet on the connection.
func (m fleetMeta) fleetSelector() string {
	return names.Selector(m.Owner, m.Cluster) + "," + names.LabelFleet + "=" + m.Hash
}

// info is the fleet.dev/info annotation of the fleet's objects: whose they
// are, in fields that are never secret, for listing orphans.
func (m fleetMeta) info(svc fleet.SharedService) map[string]string {
	v := struct {
		V       int    `json:"v"`
		Fleet   string `json:"fleet"`
		Service string `json:"service,omitempty"`
	}{1, m.Fleet, string(svc)}
	data, _ := json.Marshal(v)
	return map[string]string{names.AnnotationInfo: string(data)}
}

// anchorRef is the anchor as its dependents name it.
func anchorRef(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: name, UID: uid}
}

func (m fleetMeta) objectMeta(suffix string, labels map[string]string, owner *metav1.OwnerReference) metav1.ObjectMeta {
	om := metav1.ObjectMeta{Name: m.name(suffix), Namespace: m.Namespace, Labels: labels}
	if owner != nil {
		om.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return om
}

// anchorConfigMap is the fleet's anchor: it holds nothing, it only owns.
func anchorConfigMap(m fleetMeta) *corev1.ConfigMap {
	om := m.objectMeta(names.SuffixAnchor, m.labels(names.RoleAnchor), nil)
	om.Annotations = m.info("")
	return &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: om}
}

// netService is the fleet's headless Service: every instance StatefulSet
// names it as its serviceName, so the instance's pod <sts>-0 resolves as
// <sts>-0.<service> in the namespace, ready or not (a sibling still
// starting resolves too).
func netService(m fleetMeta, owner metav1.OwnerReference) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: m.objectMeta(names.SuffixNet, m.labels(names.RoleNet), &owner),
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{names.LabelFleet: m.Hash, names.LabelRole: string(names.RoleInstance)},
			PublishNotReadyAddresses: true,
		},
	}
}

// instanceSelector selects the fleet's instance pods.
func (m fleetMeta) instanceSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{names.LabelFleet: m.Hash, names.LabelRole: string(names.RoleInstance)}}
}

func networkPolicy(m fleetMeta, suffix string, owner metav1.OwnerReference, spec networkingv1.NetworkPolicySpec) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: m.objectMeta(suffix, m.labels(names.RolePolicy), &owner),
		Spec:       spec,
	}
}

// instancesPolicy lets the fleet's pods other than its caches (its instances
// and their Jobs) take connections only from the fleet's own instances.
// fleetd reaches them through the API server (exec, port-forward), which no
// NetworkPolicy governs.
func instancesPolicy(m fleetMeta, owner metav1.OwnerReference) *networkingv1.NetworkPolicy {
	return networkPolicy(m, names.SuffixInstancesPolicy, owner, networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{names.LabelFleet: m.Hash},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: names.LabelRole, Operator: metav1.LabelSelectorOpNotIn, Values: []string{string(names.RoleCache)},
			}},
		},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: m.instanceSelector()}}}},
	})
}

// cachesPolicy lets the fleet's caches take connections only from the
// fleet's instances, on the services' ports.
func cachesPolicy(m fleetMeta, owner metav1.OwnerReference) *networkingv1.NetworkPolicy {
	var ports []networkingv1.NetworkPolicyPort
	for _, svc := range fleet.SharedServices() {
		ports = append(ports, tcpPolicyPort(cacheSpecs[svc].port))
	}
	return networkPolicy(m, names.SuffixCachesPolicy, owner, networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{names.LabelFleet: m.Hash, names.LabelRole: string(names.RoleCache)}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From:  []networkingv1.NetworkPolicyPeer{{PodSelector: m.instanceSelector()}},
			Ports: ports,
		}},
	})
}

func tcpPolicyPort(port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: ptr(corev1.ProtocolTCP), Port: &p}
}

// egressPolicy keeps every pod of the fleet from the cloud metadata ranges
// (a pod on EKS, GKE or AKS could otherwise take the node's credentials),
// and lets it reach everything else: any address but those ranges, any pod
// (for a network plugin whose address blocks never match pods), and DNS on
// any address, so a node-local DNS cache at 169.254.20.10 still answers.
func egressPolicy(m fleetMeta, owner metav1.OwnerReference) *networkingv1.NetworkPolicy {
	return networkPolicy(m, names.SuffixEgressPolicy, owner, networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{names.LabelFleet: m.Hash}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress:      MetadataBlockRules(),
	})
}

// MetadataBlockRules are the egress rules of a fleet's pods while their
// connection blocks the cloud metadata ranges (the deep test's probe pod
// gets the same): any address but those ranges, any pod, and DNS anywhere.
func MetadataBlockRules() []networkingv1.NetworkPolicyEgressRule {
	udp53, tcp53 := intstr.FromInt32(53), intstr.FromInt32(53)
	return []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{metadataRangeV4}}},
			{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{metadataRangeV6}}},
		}},
		{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}},
		{Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: ptr(corev1.ProtocolUDP), Port: &udp53},
			{Protocol: ptr(corev1.ProtocolTCP), Port: &tcp53},
		}},
	}
}

// --- the shared services ---

// cacheSpec is how a connection runs one shared service.
type cacheSpec struct {
	suffix string
	// port is the service's, data the path of its volume in its container.
	port int32
	data string
	// size is the volume's default size.
	size func(fleet.KubeCacheSizes) string
}

// cacheSpecs are the shared services as a connection runs them.
var cacheSpecs = map[fleet.SharedService]cacheSpec{
	fleet.ServiceBuildkit: {suffix: names.SuffixBuildkit, port: 1234, data: "/var/lib/buildkit",
		size: func(s fleet.KubeCacheSizes) string { return s.Buildkit }},
	fleet.ServiceDebCache: {suffix: names.SuffixAptCache, port: 3142, data: "/var/cache/apt-cacher-ng",
		size: func(s fleet.KubeCacheSizes) string { return s.DebCache }},
	fleet.ServiceImageCache: {suffix: names.SuffixImageCache, port: 5000, data: "/var/lib/registry",
		size: func(s fleet.KubeCacheSizes) string { return s.ImageCache }},
}

// rootlessBuildkitData is the rootless buildkitd's state directory.
const rootlessBuildkitData = "/home/user/.local/share/buildkit"

func lookup(svc fleet.SharedService) (cacheSpec, error) {
	s, ok := cacheSpecs[svc]
	if !ok {
		return cacheSpec{}, fmt.Errorf("kubernetes runtime: unknown shared service %q", svc)
	}
	return s, nil
}

// cacheSize is svc's volume size on rec: the connection's cache_sizes, else
// the default.
func cacheSize(rec *fleet.KubeCluster, svc fleet.SharedService) (resource.Quantity, error) {
	s := cacheSpecs[svc]
	v := strings.TrimSpace(s.size(rec.CacheSizes))
	if v == "" {
		v = s.size(fleet.DefaultKubeCacheSizes)
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("cache_sizes.%s %q: %w", svc, v, err)
	}
	return q, nil
}

// cacheImage is svc's image on rec: the connection's service_images, else
// the pinned default. Buildkit runs rootless unless rootful.
func cacheImage(rec *fleet.KubeCluster, svc fleet.SharedService, rootful bool) string {
	img := rec.ServiceImages
	pick := func(override, def string) string {
		if v := strings.TrimSpace(override); v != "" {
			return v
		}
		return def
	}
	switch svc {
	case fleet.ServiceDebCache:
		return pick(img.DebCache, DefaultDebCacheImage)
	case fleet.ServiceImageCache:
		return pick(img.ImageCache, DefaultImageCacheImage)
	case fleet.ServiceBuildkit:
		if rootful {
			return pick(img.Buildkit, DefaultBuildkitImage)
		}
		return pick(img.BuildkitRootless, DefaultBuildkitRootlessImage)
	}
	return ""
}

// cacheVolume is svc's volume. It names no owner: it outlives the fleet's
// other objects, so the next fleet of the same name starts warm. size is the
// request, which only ever grows (an expanded volume keeps its size).
func cacheVolume(m fleetMeta, rec *fleet.KubeCluster, svc fleet.SharedService, size resource.Quantity) *corev1.PersistentVolumeClaim {
	s := cacheSpecs[svc]
	om := m.objectMeta(s.suffix, m.serviceLabels(svc), nil)
	om.Annotations = m.info(svc)
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: om,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  ptr(corev1.PersistentVolumeFilesystem),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if c := strings.TrimSpace(rec.CacheStorageClass); c != "" {
		pvc.Spec.StorageClassName = ptr(c)
	}
	return pvc
}

// cacheService is svc's ClusterIP Service, which the fleet's instances reach
// it by.
func cacheService(m fleetMeta, svc fleet.SharedService, owner metav1.OwnerReference) *corev1.Service {
	s := cacheSpecs[svc]
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: m.objectMeta(s.suffix, m.serviceLabels(svc), &owner),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{names.LabelFleet: m.Hash, names.LabelRole: string(names.RoleCache), names.LabelService: string(svc)},
			Ports:    []corev1.ServicePort{{Name: string(svc), Protocol: corev1.ProtocolTCP, Port: s.port, TargetPort: intstr.FromInt32(s.port)}},
		},
	}
}

// buildkitConfig is buildkitd.toml: its GC keeps the cache under
// gcKeepPercent of the volume, a share of the disk the cache is on, so it
// follows the volume when it grows. (Buildkit 0.17 renamed the limit:
// maxUsedSpace is the most the cache may use; gckeepstorage now sets the
// floor it never prunes below.)
func buildkitConfig(m fleetMeta, owner metav1.OwnerReference) *corev1.ConfigMap {
	toml := fmt.Sprintf(`# Written by fleet: the shared Buildkit server of fleet %q.
[worker.oci]
  gc = true
  maxUsedSpace = "%d%%"
[worker.containerd]
  enabled = false
`, m.Fleet, gcKeepPercent)
	om := m.objectMeta(names.SuffixBuildkit, m.serviceLabels(fleet.ServiceBuildkit), &owner)
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: om,
		Data:       map[string]string{"buildkitd.toml": toml},
	}
}

// deploymentInput is what a service's Deployment needs besides the fleet.
type deploymentInput struct {
	Rec *fleet.KubeCluster
	// Rootful runs buildkit privileged (the connection allows privileged
	// pods); otherwise rootless.
	Rootful bool
}

// cacheDeployment is svc's Deployment: one pod, replaced (Recreate) rather
// than rolled, since its volume mounts on one node at a time; placed like
// the connection's instances; hardened, its image pinned by digest.
func cacheDeployment(m fleetMeta, svc fleet.SharedService, owner metav1.OwnerReference, in deploymentInput) (*appsv1.Deployment, error) {
	s, err := lookup(svc)
	if err != nil {
		return nil, err
	}
	rec := in.Rec
	if rec == nil {
		rec = &fleet.KubeCluster{}
	}
	selector := names.Base(m.Owner, m.Cluster)
	selector[names.LabelFleet] = m.Hash
	selector[names.LabelRole] = string(names.RoleCache)
	selector[names.LabelService] = string(svc)

	container, volumes := serviceContainer(m, svc, s, rec, in.Rootful)
	spec := corev1.PodSpec{
		Containers:                    []corev1.Container{container},
		Volumes:                       volumes,
		RestartPolicy:                 corev1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: ptr(int64(10)),
		NodeSelector:                  copyMap(rec.NodeSelector),
		ServiceAccountName:            rec.ServiceAccount,
	}
	for _, t := range rec.Tolerations {
		spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: m.serviceLabels(svc)}, Spec: spec}
	if err := podspec.HardenTemplate(&template, podspec.HardenOptions{PinnedImages: true}); err != nil {
		return nil, fmt.Errorf("the %s server: %w", svc, err)
	}
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: m.objectMeta(s.suffix, m.serviceLabels(svc), &owner),
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr(int32(1)),
			Selector:             &metav1.LabelSelector{MatchLabels: selector},
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			RevisionHistoryLimit: ptr(int32(2)),
			Template:             template,
		},
	}, nil
}

// The volumes of a service pod.
const (
	volumeData   = "data"
	volumeConfig = "config"
)

// serviceContainer is svc's one container and the pod's volumes.
func serviceContainer(m fleetMeta, svc fleet.SharedService, s cacheSpec, rec *fleet.KubeCluster, rootful bool) (corev1.Container, []corev1.Volume) {
	volumes := []corev1.Volume{{Name: volumeData, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.name(s.suffix)},
	}}}
	c := corev1.Container{
		Name:  string(svc),
		Image: cacheImage(rec, svc, rootful),
		Ports: []corev1.ContainerPort{{Name: "service", ContainerPort: s.port, Protocol: corev1.ProtocolTCP}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		VolumeMounts:             []corev1.VolumeMount{{Name: volumeData, MountPath: s.data}},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(s.port)}},
			PeriodSeconds: 5, FailureThreshold: 3,
		},
	}
	switch svc {
	case fleet.ServiceDebCache:
		// The image's entrypoint owns the cache dir to its user, then drops
		// to it: root with the capabilities that takes, nothing more.
		c.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"},
			},
		}
	case fleet.ServiceImageCache:
		c.Env = []corev1.EnvVar{
			{Name: "REGISTRY_PROXY_REMOTEURL", Value: imageCacheUpstream},
			// The mirror's own 7-day expiry of proxied blobs needs deletes.
			{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"},
		}
		c.SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	case fleet.ServiceBuildkit:
		volumes = append(volumes, corev1.Volume{Name: volumeConfig, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: m.name(s.suffix)}},
		}})
		c.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
		if q, err := resource.ParseQuantity(orDefault(rec.MemoryLimit, fleet.DefaultKubeMemoryLimit)); err == nil {
			c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: q}
		}
		c.ReadinessProbe = nil
		if rootful {
			c.Args = []string{"--config", "/etc/buildkit/buildkitd.toml"}
			c.SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: volumeConfig, MountPath: "/etc/buildkit", ReadOnly: true})
		} else {
			uid := int64(1000)
			c.Args = []string{"--oci-worker-no-process-sandbox", "--config", "/home/user/.config/buildkit/buildkitd.toml"}
			c.VolumeMounts = []corev1.VolumeMount{
				{Name: volumeData, MountPath: rootlessBuildkitData},
				{Name: volumeConfig, MountPath: "/home/user/.config/buildkit", ReadOnly: true},
			}
			// rootlesskit needs the user namespaces seccomp and AppArmor
			// would deny it.
			c.SecurityContext = &corev1.SecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid,
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			}
		}
	}
	return c, volumes
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ptr[T any](v T) *T { return &v }
