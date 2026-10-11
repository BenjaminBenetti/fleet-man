//go:build !nok8s

package podspec

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
)

// The renderers turn a translation (Result) into the Kubernetes objects of
// one instance generation: the instance disk (a PVC), the spec and env
// Secrets, the StatefulSet, an optional PodDisruptionBudget and the seed Job.
// They are pure: the same translation and input render the same objects,
// byte for byte, so the spec hash fleet keeps is the one it recomputes.
//
// The dev container runs pod-init (Command) as root, with the instance disk
// mounted as the container's persisted root:
//
//	/<d>                       root/<d> (root-extra/<d> for a dir the image lacks)
//	/etc/resolv.conf, hostname meta/resolv.conf, meta/hostname
//	<workspace folder>         ws (a workspace bind of a folder: ws/<folder>)
//	volume targets             vol/<name>
//	/opt/fleet                 fleet
//	/opt/fleet/spec            the spec Secret, 0400
//	/opt/fleet/run             emptyDir
//	/fleet-mounts/control      emptyDir, memory-backed
//	tmpfs, --shm-size          emptyDir, memory-backed
//	/opt/fleet/docker          emptyDir shared with the fleet-docker sidecar
//	                           (a docker.sock bind; see render_docker.go)
//	socket binds               none: pod-init links an ssh-agent bind to the
//	                           agent relay, the pod agent binds the others
//
// Every workload goes through Harden.

// FieldManager is the field manager fleet writes its objects with; pod
// verification checks the StatefulSet's managedFields name it.
const FieldManager = "fleet"

// DefaultStopTimeout is the dev container's grace period when the run sets
// no --stop-timeout: docker stop's 10 seconds.
const DefaultStopTimeout = 10

// DevContainer is the instance pod's container; JobContainer a Job pod's.
const (
	DevContainer = "dev"
	JobContainer = "job"
)

// The pod annotations that keep a voluntary disruption (cluster-autoscaler
// scale-down, Karpenter consolidation) from evicting an instance: a restart
// ends every session in it. Drains are fleet's own to handle (evacuate).
const (
	annotationSafeToEvict      = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	annotationDoNotDisrupt     = "karpenter.sh/do-not-disrupt"
	annotationDefaultContainer = "kubectl.kubernetes.io/default-container"
)

// RenderInput is what the renderers need besides the translation: which
// instance generation it is, where it runs and what image the nodes pull.
type RenderInput struct {
	// Cluster is the connection (the same as Input.Cluster); nil reads as
	// one with every setting at its default.
	Cluster *fleet.KubeCluster
	// Namespace is the connection's namespace.
	Namespace string
	// Name is the instance's StatefulSet and disk name (names.Instance).
	Name string
	// Key is the instance key (names.InstanceKey), Owner the daemon's owner
	// label (names.Owner) and ClusterID the connection's record id: with
	// the fleet hash they are the labels every object carries.
	Key, Owner, ClusterID string
	// FleetName is the fleet's name and FleetHash its h8 on the connection
	// (names.FleetHash): the StatefulSet's headless Service, the share-group
	// and the SELinux level come from them.
	FleetName, FleetHash string
	// InstanceName is the instance's own name, for the disk's info
	// annotation.
	InstanceName string
	// Gen is the root generation rendered (rebuild-gen): 1 for a new
	// instance, up to names.MaxGeneration.
	Gen int
	// CreatedAt is the container-created time (the shim's inspect Created).
	CreatedAt time.Time
	// Replicas is the StatefulSet's: 1 running, 0 stopped.
	Replicas int32
	// Image is the image the nodes pull (the image delivery's result).
	Image ImageRef
	// Root is the seeded root generation's layout (the seed's manifest). An
	// empty layout renders no root mounts: what a dry-run before the seed
	// checks.
	Root RootLayout
	// StorageClass is the instance disk's StorageClass, as resolved for the
	// connection; "" is the cluster's default.
	StorageClass string
	// Identity is whom the containers run as.
	Identity Identity
	// SELinuxMount and SELinuxLevel are HardenOptions'.
	SELinuxMount bool
	SELinuxLevel string
	// Affinity holds the placement constraints (the fleet share's
	// topology); nil adds none.
	Affinity *corev1.Affinity
	// Share is where the run's fleet mounts live (share.go): the fleet's
	// share volume or the instance disk's copy. nil refuses a run with
	// fleet mounts.
	Share *Share
	// Placement places the pods by the fleet's share (internal/k8s/topology):
	// the instance pod beside it (and its running holders), the Jobs where
	// it can be used. Merged with Affinity; nil adds nothing.
	Placement *topology.Placement
	// Seed renders the generation's seed Job too; nil renders none.
	Seed *JobInput
}

// ImageRef is the image the nodes pull for an instance.
type ImageRef struct {
	// Ref is the reference the pod names: repo@sha256:<digest>, or the tag
	// a node-loaded image has.
	Ref string
	// PullPolicy is Never for a node-loaded image; "" is IfNotPresent.
	PullPolicy corev1.PullPolicy
	// PullSecrets are the pod's imagePullSecrets, by name.
	PullSecrets []string
	// ID is the image as the pod's container status reports it (the digest
	// it was delivered by, or a node-loaded image's ID): the StatefulSet's
	// image-id annotation, which pod verification compares.
	ID string
}

// RootLayout is a seeded root generation: the top-level dirs the instance
// disk persists (the seed manifest's dirs and root_extra).
type RootLayout struct {
	// Dirs are the image's top-level dirs, under root/.
	Dirs []string
	// Extra are the top-level dirs the image lacks, under root-extra/.
	Extra []string
}

// Identity is whom an instance's containers run as.
type Identity struct {
	// NonRoot runs every container without root (capability tier C: a
	// restricted namespace, or an OpenShift SCC that assigns the uid):
	// runAsNonRoot, no privilege escalation, every capability dropped but
	// the ones the run adds. Otherwise the containers run as root, as
	// docker's runtime does, and pod-init drops to containerUser.
	NonRoot bool
	// UID and GID are a NonRoot pod's; nil leaves them to the cluster (an
	// OpenShift SCC assigns both).
	UID, GID *int64
	// FSGroup owns a NonRoot pod's volumes (fsGroup, applied OnRootMismatch),
	// so its uid can write the instance disk, whose root a provisioner
	// creates root-owned, and read the spec Secret's 0400 files. nil is GID
	// (else UID) when UID is set, and left to the cluster when not.
	FSGroup *int64
}

// fsGroup is the pod's fsGroup for identity id.
func (id Identity) fsGroup() *int64 {
	switch {
	case !id.NonRoot:
		return nil
	case id.FSGroup != nil:
		return id.FSGroup
	case id.GID != nil:
		return id.GID
	}
	return id.UID
}

// Objects are one instance generation's Kubernetes objects.
type Objects struct {
	// Disk is the instance disk, named like the StatefulSet.
	Disk *corev1.PersistentVolumeClaim
	// SpecSecret holds the spec files (Secrets.Spec), EnvSecret the
	// environment values (Secrets.Env); both are named by generation.
	SpecSecret *corev1.Secret
	EnvSecret  *corev1.Secret
	// StatefulSet runs the instance pod.
	StatefulSet *appsv1.StatefulSet
	// DisruptionBudget keeps a drain from evicting the pod
	// (protect_from_drain); nil without it.
	DisruptionBudget *policyv1.PodDisruptionBudget
	// Seed is the generation's seed Job, when RenderInput.Seed asked for it.
	Seed *JobObjects
	// Warnings are differences from docker the render adds to the
	// translation's (PodSpec.Warnings), one line each.
	Warnings []string
}

// Pod is the pod the StatefulSet creates, as the API server first sees it:
// for the dry-run, and for checking a live pod against.
func (o *Objects) Pod() *corev1.Pod {
	t := o.StatefulSet.Spec.Template.DeepCopy()
	name := names.Pod(o.StatefulSet.Name)
	p := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: t.ObjectMeta,
		Spec:       t.Spec,
	}
	p.Name, p.Namespace = name, o.StatefulSet.Namespace
	p.Spec.Hostname, p.Spec.Subdomain = name, o.StatefulSet.Spec.ServiceName
	return p
}

// Render renders an instance generation's objects from its translation.
// It fails with a *Refusal (or several, joined) for what the cluster
// cannot have yet, and with a plain error for an input that is incomplete.
func Render(res *Result, in RenderInput) (*Objects, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := &renderer{res: res, pod: &res.Pod, in: in, cluster: in.Cluster}
	if r.cluster == nil {
		r.cluster = &fleet.KubeCluster{}
	}
	r.checkSupported()
	r.parseQuantities()
	if err := joinRefusals(r.refusals); err != nil {
		return nil, err
	}
	template, err := r.instanceTemplate()
	if err != nil {
		return nil, err
	}
	spec := res.Secrets.Spec
	if in.Share != nil {
		layout, err := in.Share.layoutFile()
		if err != nil {
			return nil, err
		}
		spec = maps.Clone(spec)
		if spec == nil {
			spec = map[string][]byte{}
		}
		spec[podshare.LayoutFile] = layout
	}
	o := &Objects{
		Disk:       r.disk(),
		SpecSecret: r.secret(names.SpecSecret(in.Name, in.Gen), spec),
		EnvSecret:  r.secret(names.EnvSecret(in.Name, in.Gen), res.Secrets.Env),
	}
	o.StatefulSet = r.statefulSet(template)
	if r.cluster.ProtectFromDrain {
		o.DisruptionBudget = r.disruptionBudget()
	}
	if in.Seed != nil {
		job, err := r.job(*in.Seed, names.RoleSeed)
		if err != nil {
			return nil, err
		}
		o.Seed = job
	}
	o.Warnings = r.warnings
	return o, nil
}

func (in RenderInput) validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"Namespace", in.Namespace}, {"Name", in.Name}, {"Key", in.Key}, {"Owner", in.Owner},
		{"ClusterID", in.ClusterID}, {"FleetName", in.FleetName}, {"FleetHash", in.FleetHash}, {"Image.Ref", in.Image.Ref},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("podspec: render input has no %s", strings.Join(missing, ", "))
	}
	if !names.ValidGeneration(in.Gen) {
		return fmt.Errorf("podspec: root generation %d is out of range (1-%d)", in.Gen, names.MaxGeneration)
	}
	if in.Replicas < 0 || in.Replicas > 1 {
		return fmt.Errorf("podspec: an instance runs 0 or 1 replicas, not %d", in.Replicas)
	}
	for _, d := range append(append([]string(nil), in.Root.Dirs...), in.Root.Extra...) {
		if d == "" || d == "." || d == ".." || strings.ContainsRune(d, '/') {
			return fmt.Errorf("podspec: root layout entry %q is not a top-level dir name", d)
		}
	}
	return nil
}

// renderer is one Render in progress.
type renderer struct {
	res      *Result
	pod      *PodSpec
	in       RenderInput
	cluster  *fleet.KubeCluster
	refusals []*Refusal
	warnings []string
	// requirements and diskSize are the translation's quantities, parsed.
	requirements corev1.ResourceRequirements
	diskSize     resource.Quantity
}

func (r *renderer) warn(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

func (r *renderer) refuse(code, format string, args ...any) {
	r.refusals = append(r.refusals, &Refusal{Code: code, Reason: fmt.Sprintf(format, args...)})
}

// What fleet cannot provide on a cluster yet. Each is refused by name, never
// dropped: the run would be missing what it asked for.
var unsupportedMounts = map[MountKind]string{
	MountHostDir:      "a bind of a daemon-host directory needs fleet's host-bind sync",
	MountHostFile:     "a bind of a daemon-host file needs fleet's host-bind sync",
	MountSharedVolume: "a named volume other containers share lives on the fleet's share",
}

// checkSupported refuses what the run asks for that this version of fleet
// cannot render, and connection settings it does not apply.
func (r *renderer) checkSupported() {
	for _, m := range r.pod.Mounts {
		if why, ok := unsupportedMounts[m.Kind]; ok {
			what := m.Target
			if m.Source != "" {
				what = m.Source + " at " + m.Target
			}
			r.refuse(CodeUnsupportedMount, "%s (%s): %s, which fleet does not provide on Kubernetes yet", what, m.Kind, why)
		}
	}
	for _, m := range r.pod.Mounts {
		// The translator refuses run mounts there; an image VOLUME is not
		// checked against them.
		for _, reserved := range []string{podseed.InstallDir, control.ContainerMountDir} {
			if underTarget(m.Target, reserved) {
				r.refuse(CodeUnsupportedMount, "a %s mount at %s: fleet reserves %s in every instance", m.Kind, m.Target, reserved)
			}
		}
	}
	if shared := r.fleetMountsWithoutShare(); len(shared) > 0 && r.in.Share == nil {
		r.refuse(CodeUnsupportedMount, "fleet's shared mounts (%s) live on the fleet's share, which this render was given none of", strings.Join(shared, ", "))
	}
	if r.in.Identity.NonRoot && r.pod.DooD != nil {
		// The engine runs as root, which a pod without root never has.
		r.refuse(CodeDooDNeedsPrivileged, "%s runs instances without root, and the docker engine a docker.sock bind gets (the pod's own %s sidecar) runs as root: it needs a namespace whose pod security admits privileged pods", clusterLabel(r.cluster), DockerSidecar)
	}
	if r.in.Identity.NonRoot && r.pod.Security.Privileged {
		// A pod without root never escalates (allowPrivilegeEscalation
		// false), which the API server refuses beside privileged.
		r.refuse(CodePrivileged, "%s runs instances without root, and a pod without root cannot be privileged: the devcontainer asks for --privileged, which needs a namespace whose pod security admits privileged pods", clusterLabel(r.cluster))
	}
	if strings.TrimSpace(r.cluster.PodOverridesYAML) != "" {
		r.refuse(CodeSettings, "%s has pod_overrides_yaml set, which this version of fleet does not apply to pods; clear it to create instances there", clusterLabel(r.cluster))
	}
}

func hasMountKind(mounts []Mount, k MountKind) bool {
	for _, m := range mounts {
		if m.Kind == k {
			return true
		}
	}
	return false
}

func clusterLabel(c *fleet.KubeCluster) string {
	if n := c.GetDisplayName(); n != "" {
		return n
	}
	return "the cluster"
}

// labels are the labels of the instance's objects with role.
func (r *renderer) labels(role names.Role) map[string]string {
	l := names.Base(r.in.Owner, r.in.ClusterID)
	l[names.LabelFleet] = r.in.FleetHash
	l[names.LabelKey] = r.in.Key
	l[names.LabelRole] = string(role)
	l[names.LabelShareGroup] = r.in.FleetHash
	return l
}

// selector selects the instance pod: never a Job's pod, whose role differs
// (a StatefulSet adopts every orphan pod its selector matches).
func (r *renderer) selector() *metav1.LabelSelector {
	l := names.Base(r.in.Owner, r.in.ClusterID)
	l[names.LabelKey] = r.in.Key
	l[names.LabelRole] = string(names.RoleInstance)
	return &metav1.LabelSelector{MatchLabels: l}
}

func (r *renderer) meta(name string, role names.Role) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: r.in.Namespace, Labels: r.labels(role)}
}

// diskInfo is the disk's fleet.dev/info annotation: who it belongs to, in
// fields that are never secret, for listing orphans.
type diskInfo struct {
	V        int    `json:"v"`
	Fleet    string `json:"fleet"`
	Instance string `json:"instance,omitempty"`
	Created  string `json:"created,omitempty"`
}

func (r *renderer) disk() *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: r.meta(r.in.Name, names.RoleInstance),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  ptr(corev1.PersistentVolumeFilesystem),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: r.diskSize.DeepCopy(),
			}},
		},
	}
	if r.in.StorageClass != "" {
		pvc.Spec.StorageClassName = ptr(r.in.StorageClass)
	}
	info := diskInfo{V: 1, Fleet: r.in.FleetName, Instance: r.in.InstanceName}
	if !r.in.CreatedAt.IsZero() {
		info.Created = r.in.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	data, _ := json.Marshal(info)
	pvc.Annotations = map[string]string{names.AnnotationInfo: string(data)}
	return pvc
}

func (r *renderer) secret(name string, data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: r.meta(name, names.RoleInstance),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	for k, v := range data {
		s.Data[k] = append([]byte(nil), v...)
	}
	s.Annotations = map[string]string{names.AnnotationRebuildGen: strconv.Itoa(r.in.Gen)}
	return s
}

func (r *renderer) statefulSet(template corev1.PodTemplateSpec) *appsv1.StatefulSet {
	meta := r.meta(r.in.Name, names.RoleInstance)
	meta.Annotations = map[string]string{
		names.AnnotationRebuildGen: strconv.Itoa(r.in.Gen),
		names.AnnotationSpecHash:   SpecHash(&template.Spec),
	}
	if !r.in.CreatedAt.IsZero() {
		meta.Annotations[names.AnnotationContainerCreated] = r.in.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if r.in.Image.ID != "" {
		meta.Annotations[names.AnnotationImageID] = r.in.Image.ID
	}
	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: meta,
		Spec: appsv1.StatefulSetSpec{
			Replicas:             ptr(r.in.Replicas),
			Selector:             r.selector(),
			ServiceName:          names.FleetObject(r.in.FleetName, r.in.FleetHash, names.SuffixNet),
			Template:             template,
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			RevisionHistoryLimit: ptr(int32(2)),
		},
	}
}

func (r *renderer) disruptionBudget() *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta:   metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: r.meta(r.in.Name, names.RoleInstance),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr(intstr.FromInt32(0)),
			Selector:       r.selector(),
			// A pod that is not running holds no session to protect, and a
			// crash-looping one must not block the drain.
			UnhealthyPodEvictionPolicy: ptr(policyv1.AlwaysAllow),
		},
	}
}

// hardenOptions are the Harden options of the instance's workloads.
func (r *renderer) hardenOptions() HardenOptions {
	return HardenOptions{
		ShareGroup:      r.in.FleetHash,
		SELinuxLevel:    r.in.SELinuxLevel,
		SELinuxMount:    r.in.SELinuxMount,
		AllowHostAccess: r.pod.HostNetwork || r.pod.HostIPC || r.pod.HostPID || hasMountKind(r.pod.Mounts, MountDevice),
	}
}

// The instance pod's volumes.
const (
	volumeDisk    = "disk"
	volumeSpec    = "spec"
	volumeRun     = "run"
	volumeControl = "control"
	// volumeDocker is the folder the docker engine's sidecar shares with the
	// dev container (DockerDir).
	volumeDocker = "docker"
)

// instanceTemplate is the StatefulSet's pod template, hardened.
func (r *renderer) instanceTemplate() (corev1.PodTemplateSpec, error) {
	p := r.pod
	volumes := []corev1.Volume{
		{Name: volumeDisk, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.in.Name}}},
		{Name: volumeSpec, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.SpecSecret(r.in.Name, r.in.Gen), DefaultMode: ptr(int32(0o400))}}},
		{Name: volumeRun, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volumeControl, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
	mounts, extra, err := r.instanceMounts()
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	volumes = append(volumes, extra...)

	dev := corev1.Container{
		Name:            DevContainer,
		Image:           r.in.Image.Ref,
		ImagePullPolicy: r.pullPolicy(),
		Command:         Command(),
		Env:             r.env(),
		Resources:       r.resources(),
		VolumeMounts:    mounts,
		SecurityContext: r.containerSecurity(p.Security, r.in.Identity),
		// Never the log tail: pod status is readable more widely than logs.
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Stdin:                    p.Stdin,
		TTY:                      p.TTY,
	}
	spec := r.podSpec(r.in.Identity)
	spec.Affinity = r.affinity(true)
	if sh := r.in.Share; sh != nil {
		// First: a sidecar after it starts only once it is done.
		volumes = append(volumes, sh.shareVolumes()...)
		spec.InitContainers = append(spec.InitContainers, r.shareInit(sh))
	}
	if p.DooD != nil {
		volumes = append(volumes, corev1.Volume{Name: volumeDocker, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		dev.VolumeMounts = append(dev.VolumeMounts, corev1.VolumeMount{Name: volumeDocker, MountPath: DockerDir})
		dev.Env = r.env(corev1.EnvVar{Name: DockerConfigEnv, Value: DockerConfig})
		spec.InitContainers = append(spec.InitContainers, r.dockerSidecar())
	}
	spec.Volumes = volumes
	spec.Containers = []corev1.Container{dev}
	spec.RestartPolicy = corev1.RestartPolicyAlways
	grace := int64(DefaultStopTimeout)
	if p.TerminationGracePeriodSeconds != nil {
		grace = *p.TerminationGracePeriodSeconds
	}
	spec.TerminationGracePeriodSeconds = &grace
	spec.HostNetwork, spec.HostIPC, spec.HostPID = p.HostNetwork, p.HostIPC, p.HostPID
	if p.HostNetwork {
		spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	for _, a := range p.HostAliases {
		spec.HostAliases = append(spec.HostAliases, corev1.HostAlias{IP: a.IP, Hostnames: append([]string(nil), a.Hostnames...)})
	}
	if psc := spec.SecurityContext; psc != nil {
		psc.SupplementalGroups = append([]int64(nil), p.SupplementalGroups...)
		for _, s := range p.Sysctls {
			psc.Sysctls = append(psc.Sysctls, corev1.Sysctl{Name: s.Name, Value: s.Value})
		}
	}

	t := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: r.labels(names.RoleInstance), Annotations: r.podAnnotations(DevContainer)},
		Spec:       spec,
	}
	if err := HardenTemplate(&t, r.hardenOptions()); err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	return t, nil
}

// affinity is a pod's affinity: the render's own and the share's placement
// (holder: the pod mounts the share, so it goes beside the running pods that
// do).
func (r *renderer) affinity(holder bool) *corev1.Affinity {
	var p *corev1.Affinity
	if r.in.Placement != nil {
		p = r.in.Placement.Affinity(holder)
	}
	return topology.Merge(r.in.Affinity, p)
}

// podAnnotations are a pod's annotations: what the translation needs (the
// sysbox user namespace mode), the disruption guards, and the container
// kubectl picks.
func (r *renderer) podAnnotations(container string) map[string]string {
	a := map[string]string{
		annotationSafeToEvict:      "false",
		annotationDoNotDisrupt:     "true",
		annotationDefaultContainer: container,
	}
	for k, v := range r.pod.Annotations {
		a[k] = v
	}
	return a
}

// podSpec is what every pod of the instance shares: placement (the node
// selector, tolerations, affinity and runtime class), the pull secrets, the
// service account, DNS, and the pod's identity id.
func (r *renderer) podSpec(id Identity) corev1.PodSpec {
	p := r.pod
	spec := corev1.PodSpec{
		DNSPolicy:          corev1.DNSClusterFirst,
		ServiceAccountName: r.cluster.ServiceAccount,
		SecurityContext:    podSecurity(id),
	}
	if len(p.NodeSelector) > 0 {
		spec.NodeSelector = map[string]string{}
		for k, v := range p.NodeSelector {
			spec.NodeSelector[k] = v
		}
	}
	for _, s := range r.in.Image.PullSecrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}
	for _, t := range r.cluster.Tolerations {
		spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
			Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value,
			Effect: corev1.TaintEffect(t.Effect), TolerationSeconds: t.TolerationSeconds,
		})
	}
	// The pod's own runtime class goes to its Jobs too: a RuntimeClass can
	// carry scheduling (a node pool), and a Job that binds the instance disk
	// must land where the instance can run.
	if p.RuntimeClassName != "" {
		spec.RuntimeClassName = ptr(p.RuntimeClassName)
	}
	if d := p.DNS; d != nil {
		cfg := &corev1.PodDNSConfig{Nameservers: append([]string(nil), d.Nameservers...), Searches: append([]string(nil), d.Searches...)}
		for _, o := range d.Options {
			cfg.Options = append(cfg.Options, corev1.PodDNSConfigOption{Name: o.Name, Value: o.Value})
		}
		spec.DNSConfig = cfg
	}
	return spec
}

func podSecurity(id Identity) *corev1.PodSecurityContext {
	psc := &corev1.PodSecurityContext{}
	if id.NonRoot {
		psc.RunAsNonRoot = ptr(true)
		psc.RunAsUser, psc.RunAsGroup, psc.FSGroup = id.UID, id.GID, id.fsGroup()
	}
	return psc
}

func (r *renderer) pullPolicy() corev1.PullPolicy {
	if r.in.Image.PullPolicy == "" {
		return corev1.PullIfNotPresent
	}
	return r.in.Image.PullPolicy
}

// env is the dev container's environment: the run's, every value from the
// env Secret, so no value reaches the kubelet's $(VAR) expansion; fleet's
// own (fixed paths, such as DockerConfigEnv); then the pod's uid for
// pod-init (podinit.PodUIDEnv, which it keeps from the start script), last
// so it wins over a run variable of that name.
func (r *renderer) env(fleets ...corev1.EnvVar) []corev1.EnvVar {
	secret := names.EnvSecret(r.in.Name, r.in.Gen)
	var env []corev1.EnvVar
	for _, e := range r.pod.Env {
		env = append(env, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: e.Key},
		}})
	}
	env = append(env, fleets...)
	return append(env, corev1.EnvVar{Name: podinit.PodUIDEnv, ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"},
	}})
}

// resources are the dev container's requests and limits (parseQuantities
// read them).
func (r *renderer) resources() corev1.ResourceRequirements {
	return *r.requirements.DeepCopy()
}

// parseQuantities reads the translation's quantities as the API takes them:
// the resources and the disk size. A connection setting the translation
// read but the API does not (a size docker spells but Kubernetes does not)
// is refused.
func (r *renderer) parseQuantities() {
	parse := func(what, v string) (resource.Quantity, bool) {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			r.refuse(CodeSettings, "%s %q is not a Kubernetes quantity: %v", what, v, err)
			return resource.Quantity{}, false
		}
		return q, true
	}
	conv := func(what string, in map[string]string) corev1.ResourceList {
		if len(in) == 0 {
			return nil
		}
		l := corev1.ResourceList{}
		for _, k := range sortedKeys(in) {
			if q, ok := parse(what+" "+k, in[k]); ok {
				l[corev1.ResourceName(k)] = q
			}
		}
		return l
	}
	r.requirements.Requests = conv("the request", r.pod.Resources.Requests)
	r.requirements.Limits = conv("the limit", r.pod.Resources.Limits)
	r.diskSize, _ = parse("the disk size", orDefault(r.pod.DiskSize, fleet.DefaultKubeDiskSize))
}

// dockerDefaultCaps are the capabilities docker's runtime gives every
// container that Kubernetes' baseline pod security also admits (docker's
// NET_RAW is the one it does not). They are added explicitly, so a container
// gets them on runtimes whose default set is smaller (CRI-O lacks MKNOD,
// SETFCAP, SYS_CHROOT and AUDIT_WRITE): the seed needs SETFCAP and MKNOD
// to copy file capabilities and device nodes, and the devcontainer expects
// what docker gave it.
var dockerDefaultCaps = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// containerSecurity is a container's security context for s (the
// translation's for the dev container, none for a Job's) and identity id.
func (r *renderer) containerSecurity(s Security, id Identity) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{}
	var add, drop []string
	if id.NonRoot {
		sc.RunAsNonRoot = ptr(true)
		sc.RunAsUser, sc.RunAsGroup = id.UID, id.GID
		sc.AllowPrivilegeEscalation = ptr(false)
		drop = []string{"ALL"}
		add = append(add, s.CapAdd...)
	} else {
		sc.RunAsUser, sc.RunAsGroup = ptr(int64(0)), ptr(int64(0))
		if !s.Privileged && !containsString(s.CapDrop, "ALL") {
			for _, c := range dockerDefaultCaps {
				if !containsString(s.CapDrop, c) {
					add = append(add, c)
				}
			}
		}
		for _, c := range s.CapAdd {
			add = appendUnique(add, c)
		}
		drop = append(drop, s.CapDrop...)
	}
	if s.Privileged {
		sc.Privileged = ptr(true)
	}
	if len(add) > 0 || len(drop) > 0 {
		sc.Capabilities = &corev1.Capabilities{}
		for _, c := range add {
			sc.Capabilities.Add = append(sc.Capabilities.Add, corev1.Capability(c))
		}
		for _, c := range drop {
			sc.Capabilities.Drop = append(sc.Capabilities.Drop, corev1.Capability(c))
		}
	}
	if s.Seccomp == SeccompUnconfined {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	}
	switch {
	case s.AppArmor == AppArmorUnconfined:
		sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	case s.AppArmor == AppArmorRuntimeDefault:
		sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}
	case strings.HasPrefix(s.AppArmor, AppArmorLocalhostPrefix):
		sc.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeLocalhost, LocalhostProfile: ptr(strings.TrimPrefix(s.AppArmor, AppArmorLocalhostPrefix))}
	}
	if se := s.SELinux; se != nil {
		sc.SELinuxOptions = &corev1.SELinuxOptions{User: se.User, Role: se.Role, Type: se.Type}
	}
	if s.NoNewPrivileges && !id.NonRoot {
		// Kubernetes refuses allowPrivilegeEscalation=false beside privileged
		// (and beside a capability spelled CAP_SYS_ADMIN, which the
		// translation never writes: SYS_ADMIN beside it is admitted, and
		// runs with no_new_privs as on docker).
		if s.Privileged {
			r.warn("--security-opt no-new-privileges: a privileged pod cannot refuse privilege escalation; it is not applied")
		} else {
			sc.AllowPrivilegeEscalation = ptr(false)
		}
	}
	if s.ReadOnlyRootFilesystem {
		sc.ReadOnlyRootFilesystem = ptr(true)
	}
	return sc
}

// instanceMounts are the dev container's volume mounts, and the volumes the
// run's tmpfs and devices need. A run mount at a path the persisted root
// would mount (a top-level dir, /etc/resolv.conf, /etc/hostname) replaces
// that mount, as a docker volume over the image's dir hides it.
func (r *renderer) instanceMounts() ([]corev1.VolumeMount, []corev1.Volume, error) {
	p := r.pod
	taken := map[string]bool{}
	for _, m := range p.Mounts {
		taken[m.Target] = true
	}
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	add := func(m corev1.VolumeMount) {
		mounts = append(mounts, m)
	}
	rootMount := func(dirs []string, subPath func(string) string) {
		for _, d := range sortedUnique(dirs) {
			if alwaysEphemeral[d] || taken["/"+d] {
				continue
			}
			add(corev1.VolumeMount{Name: volumeDisk, MountPath: "/" + d, SubPath: subPath(d), ReadOnly: p.ReadOnlyRoot})
		}
	}
	rootMount(r.in.Root.Dirs, podseed.RootSubPath)
	rootMount(r.in.Root.Extra, podseed.RootExtraSubPath)
	for _, f := range []struct{ target, subPath string }{{"/etc/resolv.conf", podseed.ResolvConfFile}, {"/etc/hostname", podseed.HostnameFile}} {
		if !taken[f.target] {
			add(corev1.VolumeMount{Name: volumeDisk, MountPath: f.target, SubPath: f.subPath})
		}
	}
	add(corev1.VolumeMount{Name: volumeDisk, MountPath: podseed.InstallDir, SubPath: podseed.DirFleet})
	add(corev1.VolumeMount{Name: volumeSpec, MountPath: podseed.SpecDir, ReadOnly: true})
	add(corev1.VolumeMount{Name: volumeRun, MountPath: podseed.RunDir})
	add(corev1.VolumeMount{Name: volumeControl, MountPath: control.ContainerMountDir})
	if sh := r.in.Share; sh != nil {
		add(sh.treeMount())
	}

	for _, m := range p.Mounts {
		switch m.Kind {
		case MountWorkspace:
			sub := podseed.DirWorkspace
			if m.SubPath != "" {
				sub = path.Join(podseed.DirWorkspace, m.SubPath)
			}
			add(corev1.VolumeMount{Name: volumeDisk, MountPath: m.Target, SubPath: sub, ReadOnly: m.ReadOnly})
		case MountVolume:
			add(corev1.VolumeMount{Name: volumeDisk, MountPath: m.Target, SubPath: m.SubPath, ReadOnly: m.ReadOnly})
		case MountTmpfs:
			name := "tmpfs-" + sha8(m.Target)
			ed := &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}
			if m.SizeLimit != "" {
				q, err := resource.ParseQuantity(m.SizeLimit)
				if err != nil {
					return nil, nil, fmt.Errorf("podspec: tmpfs size %q at %s: %w", m.SizeLimit, m.Target, err)
				}
				ed.SizeLimit = &q
			}
			volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: ed}})
			add(corev1.VolumeMount{Name: name, MountPath: m.Target, ReadOnly: m.ReadOnly})
		case MountDevice:
			name := "dev-" + sha8(m.Target)
			volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: m.Source, Type: ptr(corev1.HostPathCharDev)}}})
			add(corev1.VolumeMount{Name: name, MountPath: m.Target})
		case MountDockerSocket:
			// pod-init links the target to the sidecar's socket
			// (pid1.json links), which the dev container sees in the
			// folder they share.
			if p.DooD == nil {
				return nil, nil, fmt.Errorf("podspec: a docker socket mount at %s without the docker engine's sidecar", m.Target)
			}
		case MountFleet:
			// checkSupported refused one without a share.
			add(r.in.Share.fleetMount(m))
		case MountSSHAgent, MountHostSocket, MountHostSocketDir:
			// No volume: pod-init links an ssh-agent bind's target to the
			// agent relay (pid1.json links), and the pod agent binds the
			// other sockets at their targets (PodSpec.HostSockets).
		default:
			// checkSupported refused every other kind.
			return nil, nil, fmt.Errorf("podspec: no rendering for a %s mount", m.Kind)
		}
	}
	seen := map[string]bool{}
	for _, m := range mounts {
		if seen[m.MountPath] {
			return nil, nil, fmt.Errorf("podspec: two mounts at %s", m.MountPath)
		}
		seen[m.MountPath] = true
	}
	return mounts, volumes, nil
}

// alwaysEphemeral are top-level dirs a container never persists: the
// kernel's and the runtime's.
var alwaysEphemeral = map[string]bool{"proc": true, "sys": true, "dev": true}

func sortedUnique(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			out[n] = s
			n++
		}
	}
	return out[:n]
}

func ptr[T any](v T) *T { return &v }
