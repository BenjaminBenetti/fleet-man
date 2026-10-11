//go:build !nok8s

package podspec

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// clone.go renders a clone's objects from its source's, as docker commit and
// run make a container from another's: the same pod template (the image by
// digest, the security context, the mounts, the placement), the same spec
// and env Secrets, the same container-created time (so the lifecycle hooks'
// markers, which the clone's disk copied, stay true), and a disk of the same
// size and class. Only who the objects are changes: the clone's name, key and
// labels, and the host workspace folder its devcontainer id labels name. No
// translation runs again: the source's verified render is the clone's.

// CloneSource is the source instance's objects as fleetd read them (and
// verified against its record): what the clone's are made from.
type CloneSource struct {
	StatefulSet           *appsv1.StatefulSet
	SpecSecret, EnvSecret *corev1.Secret
	Disk                  *corev1.PersistentVolumeClaim
	// Gen is the source's root generation, which the clone keeps.
	Gen int
}

// CloneInput is who the clone is.
type CloneInput struct {
	Cluster *fleet.KubeCluster
	// Namespace, Name (the clone's StatefulSet and disk), Key, Owner and
	// ClusterID are RenderInput's, for the clone.
	Namespace, Name, Key, Owner, ClusterID string
	// FleetName and FleetHash are the fleet's (the source's).
	FleetName, FleetHash string
	InstanceName         string
	// WorkspaceDir is the clone's host workspace folder: the devcontainer id
	// labels the pod-mode shim answers inspect with are rebased onto it, as
	// a docker clone's labels are.
	WorkspaceDir string
	// Replicas is the clone StatefulSet's (1: it starts).
	Replicas int32
	// DiskSource restores the clone's disk from a snapshot of the source's
	// disk, or clones it from that disk; nil makes an empty one (a copier
	// fills it).
	DiskSource *corev1.TypedLocalObjectReference
	// DiskSize, when set, is the clone disk's size (a snapshot's restore
	// size above the source's request); "" is the source disk's.
	DiskSize string
}

// CloneObjects renders the clone's disk, Secrets, StatefulSet and
// disruption budget from the source's. The pod template is the source's but
// for the objects it names (its disk, its spec and env Secrets) and its
// labels; the render fails when anything in it still names the source.
func CloneObjects(src CloneSource, in CloneInput) (*Objects, error) {
	if src.StatefulSet == nil || src.SpecSecret == nil || src.EnvSecret == nil || src.Disk == nil {
		return nil, fmt.Errorf("podspec: a clone needs its source's StatefulSet, spec and env Secrets and disk")
	}
	if !names.ValidGeneration(src.Gen) {
		return nil, fmt.Errorf("podspec: root generation %d is out of range", src.Gen)
	}
	srcName := src.StatefulSet.Name
	if in.Name == "" || in.Name == srcName || in.Key == "" || in.Namespace == "" || in.Owner == "" || in.ClusterID == "" || in.FleetHash == "" {
		return nil, fmt.Errorf("podspec: clone input incomplete (name %q, key %q)", in.Name, in.Key)
	}
	r := in.renderer(src.Gen)

	tmpl := src.StatefulSet.Spec.Template.DeepCopy()
	tmpl.Labels = r.labels(names.RoleInstance)
	renamed := map[string]string{
		srcName:                            in.Name,
		names.SpecSecret(srcName, src.Gen): names.SpecSecret(in.Name, src.Gen),
		names.EnvSecret(srcName, src.Gen):  names.EnvSecret(in.Name, src.Gen),
		src.SpecSecret.Name:                names.SpecSecret(in.Name, src.Gen),
		src.EnvSecret.Name:                 names.EnvSecret(in.Name, src.Gen),
	}
	rename := func(n string) string {
		if to, ok := renamed[n]; ok {
			return to
		}
		return n
	}
	for i := range tmpl.Spec.Volumes {
		v := &tmpl.Spec.Volumes[i]
		if v.PersistentVolumeClaim != nil {
			v.PersistentVolumeClaim.ClaimName = rename(v.PersistentVolumeClaim.ClaimName)
		}
		if v.Secret != nil {
			v.Secret.SecretName = rename(v.Secret.SecretName)
		}
	}
	forEachContainer(&tmpl.Spec, func(c *corev1.Container) {
		for i := range c.Env {
			if ref := c.Env[i].ValueFrom; ref != nil && ref.SecretKeyRef != nil {
				ref.SecretKeyRef.Name = rename(ref.SecretKeyRef.Name)
			}
		}
		for i := range c.EnvFrom {
			if s := c.EnvFrom[i].SecretRef; s != nil {
				s.Name = rename(s.Name)
			}
		}
	})
	if left := namesSource(tmpl, srcName); left != "" {
		return nil, fmt.Errorf("podspec: the clone's pod template still names the source (%s)", left)
	}

	o := &Objects{}
	disk, err := r.cloneDisk(src.Disk, in)
	if err != nil {
		return nil, err
	}
	o.Disk = disk
	spec, err := cloneSpecFiles(src.SpecSecret.Data, in.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	o.SpecSecret = r.secret(names.SpecSecret(in.Name, src.Gen), spec)
	o.EnvSecret = r.secret(names.EnvSecret(in.Name, src.Gen), src.EnvSecret.Data)

	sts := r.statefulSet(*tmpl)
	for _, k := range []string{names.AnnotationContainerCreated, names.AnnotationImageID} {
		if v, ok := src.StatefulSet.Annotations[k]; ok {
			sts.Annotations[k] = v
		}
	}
	sts.Spec.ServiceName = src.StatefulSet.Spec.ServiceName
	o.StatefulSet = sts
	if r.cluster.ProtectFromDrain {
		o.DisruptionBudget = r.disruptionBudget()
	}
	return o, nil
}

// renderer is a renderer of the clone's objects: no translation behind it,
// only who the objects are.
func (in CloneInput) renderer(gen int) *renderer {
	cluster := in.Cluster
	if cluster == nil {
		cluster = &fleet.KubeCluster{}
	}
	return &renderer{res: &Result{}, pod: &PodSpec{}, cluster: cluster, in: RenderInput{
		Cluster: cluster, Namespace: in.Namespace, Name: in.Name, Key: in.Key, Owner: in.Owner, ClusterID: in.ClusterID,
		FleetName: in.FleetName, FleetHash: in.FleetHash, InstanceName: in.InstanceName, Gen: gen, Replicas: in.Replicas,
		SELinuxLevel: SELinuxLevel(in.FleetHash),
	}}
}

// Labels are the labels of the clone's objects with role (a copier Job's, a
// snapshot's).
func (in CloneInput) Labels(role names.Role) map[string]string {
	return in.renderer(1).labels(role)
}

// cloneDisk is the clone's disk: the source's size (or the restore size),
// class and modes, restored from in.DiskSource when set.
func (r *renderer) cloneDisk(src *corev1.PersistentVolumeClaim, in CloneInput) (*corev1.PersistentVolumeClaim, error) {
	size, ok := src.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return nil, fmt.Errorf("podspec: the source disk %s has no storage request", src.Name)
	}
	if in.DiskSize != "" {
		q, err := resource.ParseQuantity(in.DiskSize)
		if err != nil {
			return nil, fmt.Errorf("podspec: clone disk size %q: %w", in.DiskSize, err)
		}
		if q.Cmp(size) > 0 {
			size = q
		}
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: r.meta(in.Name, names.RoleInstance),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      append([]corev1.PersistentVolumeAccessMode(nil), src.Spec.AccessModes...),
			VolumeMode:       src.Spec.VolumeMode,
			StorageClassName: src.Spec.StorageClassName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size.DeepCopy()}},
		},
	}
	if in.DiskSource != nil {
		ds := *in.DiskSource
		pvc.Spec.DataSource = &ds
		pvc.Spec.DataSourceRef = &corev1.TypedObjectReference{APIGroup: ds.APIGroup, Kind: ds.Kind, Name: ds.Name}
	}
	created := ""
	if info := src.Annotations[names.AnnotationInfo]; info != "" {
		var di diskInfo
		if json.Unmarshal([]byte(info), &di) == nil {
			created = di.Created
		}
	}
	data, _ := json.Marshal(diskInfo{V: 1, Fleet: in.FleetName, Instance: in.InstanceName, Created: created})
	pvc.Annotations = map[string]string{names.AnnotationInfo: string(data)}
	return pvc, nil
}

// cloneSpecFiles are the source's spec files for the clone: its inspect
// answer's devcontainer id labels rebased onto the clone's host workspace.
func cloneSpecFiles(data map[string][]byte, wsDir string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(data))
	for k, v := range data {
		out[k] = append([]byte(nil), v...)
	}
	raw, ok := out[InspectFile]
	if !ok || wsDir == "" {
		return out, nil
	}
	var c kubeshim.Container
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("podspec: the source's %s: %w", InspectFile, err)
	}
	if rebaseLabels(c.Labels, wsDir) {
		b, err := marshalFile(c)
		if err != nil {
			return nil, err
		}
		out[InspectFile] = b
	}
	return out, nil
}

// rebaseLabels points devcontainer.local_folder at wsDir and moves
// devcontainer.config_file under it, as a docker clone's run does.
func rebaseLabels(labels map[string]string, wsDir string) bool {
	old, ok := labels[devmeta.LocalFolderLabel]
	if !ok || old == wsDir {
		return false
	}
	labels[devmeta.LocalFolderLabel] = wsDir
	if cf, ok := labels[devmeta.ConfigFileLabel]; ok && strings.HasPrefix(cf, old+"/") {
		labels[devmeta.ConfigFileLabel] = wsDir + strings.TrimPrefix(cf, old)
	}
	return true
}

// namesSource reports a field of the template that still names the source
// StatefulSet's objects: its disk, or a Secret of it.
func namesSource(t *corev1.PodTemplateSpec, src string) string {
	for _, v := range t.Spec.Volumes {
		if pvc := v.PersistentVolumeClaim; pvc != nil && (pvc.ClaimName == src || strings.HasPrefix(pvc.ClaimName, src+"-")) {
			return "volume " + v.Name
		}
		if s := v.Secret; s != nil && strings.HasPrefix(s.SecretName, src+"-") {
			return "volume " + v.Name
		}
	}
	var left string
	forEachContainer(&t.Spec, func(c *corev1.Container) {
		for _, e := range c.Env {
			if ref := e.ValueFrom; ref != nil && ref.SecretKeyRef != nil && strings.HasPrefix(ref.SecretKeyRef.Name, src+"-") && left == "" {
				left = "env " + e.Name
			}
		}
	})
	return left
}

// CloneJob renders a Job of the clone (its copier, or the fix-ups of a
// restored disk) from the source's pod template: its image, placement,
// pull secrets, DNS and pod security, the clone's disk at the job's disk
// path, and job's other claims (the source's disk, for a copier).
func CloneJob(src *appsv1.StatefulSet, in CloneInput, gen int, job JobInput, role names.Role) (*JobObjects, error) {
	if src == nil || len(src.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("podspec: a clone Job needs the source's pod template")
	}
	r := in.renderer(gen)
	dev := src.Spec.Template.Spec.Containers[0]
	for _, c := range src.Spec.Template.Spec.Containers {
		if c.Name == DevContainer {
			dev = c
		}
	}
	r.in.Image = ImageRef{Ref: dev.Image, PullPolicy: dev.ImagePullPolicy}
	r.in.Identity = identityOf(&src.Spec.Template.Spec)
	tspec := src.Spec.Template.Spec
	job.From = &tspec
	if err := r.in.validate(); err != nil {
		return nil, err
	}
	return r.job(job, role)
}

// identityOf is whom a rendered pod runs as (its pod security context).
func identityOf(spec *corev1.PodSpec) Identity {
	psc := spec.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		return Identity{}
	}
	return Identity{NonRoot: true, UID: psc.RunAsUser, GID: psc.RunAsGroup, FSGroup: psc.FSGroup}
}

// fromPlacement gives a Job's pod spec what a rendered instance pod's says of
// where and how its pods run: placement, runtime class, pull secrets, DNS,
// service account, and the pod's identity.
func fromPlacement(spec *corev1.PodSpec, from *corev1.PodSpec) {
	spec.NodeSelector = nil
	if len(from.NodeSelector) > 0 {
		spec.NodeSelector = map[string]string{}
		for k, v := range from.NodeSelector {
			spec.NodeSelector[k] = v
		}
	}
	spec.Tolerations = append([]corev1.Toleration(nil), from.Tolerations...)
	spec.Affinity = from.Affinity.DeepCopy()
	spec.RuntimeClassName = from.RuntimeClassName
	spec.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), from.ImagePullSecrets...)
	spec.DNSConfig = from.DNSConfig.DeepCopy()
	if from.DNSPolicy != "" && from.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		spec.DNSPolicy = from.DNSPolicy
	}
	spec.ServiceAccountName = from.ServiceAccountName
	if from.SecurityContext != nil {
		psc := from.SecurityContext.DeepCopy()
		psc.Sysctls, psc.SupplementalGroups = nil, nil
		spec.SecurityContext = psc
	}
}

// pinToNode adds a required node affinity to the Job's pod: it runs on node
// (a copier beside the running source, whose disk is attached there).
func pinToNode(spec *corev1.PodSpec, node string) {
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := spec.Affinity.NodeAffinity
	term := corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil || len(na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{term}}}}
		return
	}
	// Terms are ORed: the node goes into each, so every way the pod may be
	// placed also puts it on that node.
	for i := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		t := &na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
		t.MatchFields = append(t.MatchFields, term)
	}
}

// CloneCreated is a source StatefulSet's container-created time, which its
// clone keeps; zero when it has none.
func CloneCreated(sts *appsv1.StatefulSet) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, sts.Annotations[names.AnnotationContainerCreated])
	return t
}

// GenOf is a rendered StatefulSet's root generation (its rebuild-gen), 0
// when it has none.
func GenOf(sts *appsv1.StatefulSet) int {
	n, _ := strconv.Atoi(sts.Annotations[names.AnnotationRebuildGen])
	return n
}

// CloneSpecOf is what a clone's job does for the clone whose objects are o
// besides its copy: the clone pod's hostname, its mount targets, and its
// volumes, read off the pod template (no translation is behind a clone). A
// volume mounted whole copies up from the image, as a new docker volume does;
// one mounted by a subpath of it does not.
func CloneSpecOf(o *Objects) podseed.CloneSpec {
	seed := SeedSpec(&Result{}, o)
	s := podseed.CloneSpec{Hostname: seed.Hostname, MountDirs: seed.MountDirs}
	seen := map[string]bool{}
	for _, m := range o.StatefulSet.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name != volumeDisk || !strings.HasPrefix(m.SubPath, podseed.DirVolumes+"/") {
			continue
		}
		name, rest, _ := strings.Cut(strings.TrimPrefix(m.SubPath, podseed.DirVolumes+"/"), "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		s.Volumes = append(s.Volumes, podseed.Volume{Name: name, Target: m.MountPath, CopyUp: rest == ""})
	}
	return s
}
