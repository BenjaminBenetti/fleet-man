//go:build !nok8s

package podspec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// cloneSource renders a source instance (env, a volume, a privileged
// security context) and returns its objects as a clone reads them.
func cloneSource(t *testing.T) (CloneSource, RenderInput) {
	t.Helper()
	var ri RenderInput
	o := mustRender(t, []string{"-e", "TOKEN", "--privileged", "-v", "/var/lib/docker"}, func(in *Input, r *RenderInput) {
		in.Capture.Env = []kubeshim.EnvVar{{Name: "TOKEN", Value: "secret"}}
		r.Seed = nil
		in.Cluster.ProtectFromDrain = true
		ri = *r
	})
	return CloneSource{StatefulSet: o.StatefulSet, SpecSecret: o.SpecSecret, EnvSecret: o.EnvSecret, Disk: o.Disk, Gen: 1}, ri
}

func cloneInputFor(r RenderInput) CloneInput {
	created := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	key := names.InstanceKey(testDaemonID, testFleet, "beta", created)
	return CloneInput{
		Cluster: r.Cluster, Namespace: r.Namespace, Name: names.Instance(testFleet, "beta", key), Key: key,
		Owner: r.Owner, ClusterID: r.ClusterID, FleetName: r.FleetName, FleetHash: r.FleetHash, InstanceName: "beta",
		WorkspaceDir: "/home/u/.fleet/workspaces/fleet-man/beta/fleet-man", Replicas: 1,
	}
}

func TestCloneObjectsAreTheSourcesUnderTheClonesName(t *testing.T) {
	src, r := cloneSource(t)
	in := cloneInputFor(r)
	o, err := CloneObjects(src, in)
	if err != nil {
		t.Fatal(err)
	}
	sts := o.StatefulSet
	if sts.Name != in.Name || o.Disk.Name != in.Name || o.SpecSecret.Name != names.SpecSecret(in.Name, 1) || o.EnvSecret.Name != names.EnvSecret(in.Name, 1) {
		t.Fatalf("names: sts %s disk %s spec %s env %s", sts.Name, o.Disk.Name, o.SpecSecret.Name, o.EnvSecret.Name)
	}
	ri := r
	ri.Name, ri.Key, ri.InstanceName = in.Name, in.Key, in.InstanceName
	for what, l := range map[string]map[string]string{"sts": sts.Labels, "template": sts.Spec.Template.Labels, "disk": o.Disk.Labels, "spec": o.SpecSecret.Labels, "pdb": o.DisruptionBudget.Labels} {
		checkLabels(t, what, l, ri)
	}
	if !selects(sts.Spec.Selector, sts.Spec.Template.Labels) || selects(sts.Spec.Selector, src.StatefulSet.Spec.Template.Labels) {
		t.Fatal("the clone's selector picks the source's pods, or not its own")
	}
	// The pod is the source's: image, security, mounts, resources.
	sdev, cdev := devOf(&Objects{StatefulSet: src.StatefulSet}), devOf(o)
	if cdev.Image != sdev.Image || jsonString(cdev.SecurityContext) != jsonString(sdev.SecurityContext) || jsonString(cdev.VolumeMounts) != jsonString(sdev.VolumeMounts) || jsonString(cdev.Resources) != jsonString(sdev.Resources) {
		t.Fatal("the clone's dev container differs from the source's")
	}
	if !*cdev.SecurityContext.Privileged {
		t.Fatal("the clone dropped the source's privilege")
	}
	for _, e := range cdev.Env {
		if ref := e.ValueFrom.SecretKeyRef; ref != nil && ref.Name != o.EnvSecret.Name {
			t.Errorf("env %s reads Secret %s", e.Name, ref.Name)
		}
	}
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != in.Name {
			t.Errorf("volume %s mounts %s", v.Name, v.PersistentVolumeClaim.ClaimName)
		}
		if v.Secret != nil && v.Secret.SecretName != o.SpecSecret.Name {
			t.Errorf("volume %s mounts Secret %s", v.Name, v.Secret.SecretName)
		}
	}
	for _, k := range []string{names.AnnotationContainerCreated, names.AnnotationImageID} {
		if sts.Annotations[k] == "" || sts.Annotations[k] != src.StatefulSet.Annotations[k] {
			t.Errorf("annotation %s = %q, the source's %q", k, sts.Annotations[k], src.StatefulSet.Annotations[k])
		}
	}
	if sts.Annotations[names.AnnotationSpecHash] != SpecHash(&sts.Spec.Template.Spec) || sts.Annotations[names.AnnotationRebuildGen] != "1" {
		t.Errorf("annotations %v", sts.Annotations)
	}
	if *sts.Spec.Replicas != 1 || sts.Spec.ServiceName != src.StatefulSet.Spec.ServiceName {
		t.Errorf("replicas %d service %s", *sts.Spec.Replicas, sts.Spec.ServiceName)
	}
	// The disk: the source's size and class, no data source.
	if o.Disk.Spec.Resources.Requests.Storage().Cmp(*src.Disk.Spec.Resources.Requests.Storage()) != 0 || o.Disk.Spec.DataSource != nil {
		t.Errorf("disk %+v", o.Disk.Spec)
	}
	var info diskInfo
	if err := json.Unmarshal([]byte(o.Disk.Annotations[names.AnnotationInfo]), &info); err != nil || info.Instance != "beta" {
		t.Errorf("disk info %q", o.Disk.Annotations[names.AnnotationInfo])
	}
	// The env Secret's values are the source's; the inspect answer's id
	// labels name the clone's workspace.
	if string(o.EnvSecret.Data[envKey("TOKEN")]) != "secret" {
		t.Error("the env Secret lost the source's values")
	}
	var c kubeshim.Container
	if err := json.Unmarshal(o.SpecSecret.Data[InspectFile], &c); err != nil {
		t.Fatal(err)
	}
	if c.Labels[devmeta.LocalFolderLabel] != in.WorkspaceDir || c.Labels[devmeta.ConfigFileLabel] != in.WorkspaceDir+"/.devcontainer/devcontainer.json" {
		t.Errorf("inspect labels %v", c.Labels)
	}
	if string(o.SpecSecret.Data[PID1File]) != string(src.SpecSecret.Data[PID1File]) {
		t.Error("pid1.json changed")
	}
	if strings.Contains(jsonString(o.StatefulSet), src.StatefulSet.Name) {
		t.Error("the clone's StatefulSet still names the source")
	}
}

func TestCloneDiskFromASnapshot(t *testing.T) {
	src, r := cloneSource(t)
	in := cloneInputFor(r)
	group := "snapshot.storage.k8s.io"
	in.DiskSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: names.CloneSnapshot(in.Name, 1)}
	in.DiskSize = "80Gi"
	o, err := CloneObjects(src, in)
	if err != nil {
		t.Fatal(err)
	}
	ds := o.Disk.Spec.DataSource
	if ds == nil || ds.Kind != "VolumeSnapshot" || *ds.APIGroup != group || o.Disk.Spec.DataSourceRef == nil || o.Disk.Spec.DataSourceRef.Name != ds.Name {
		t.Fatalf("data source %+v ref %+v", ds, o.Disk.Spec.DataSourceRef)
	}
	if got := o.Disk.Spec.Resources.Requests.Storage().String(); got != "80Gi" {
		t.Errorf("size %s, want the snapshot's restore size", got)
	}
	// A restore size below the source's request keeps the request.
	in.DiskSize = "1Gi"
	o, _ = CloneObjects(src, in)
	if o.Disk.Spec.Resources.Requests.Storage().Cmp(*src.Disk.Spec.Resources.Requests.Storage()) != 0 {
		t.Errorf("size %s", o.Disk.Spec.Resources.Requests.Storage())
	}
}

// A driver known to clone volumes clones a stopped instance's disk (T1b)
// unasked; one only known to snapshot does not.
func TestClonesVolumes(t *testing.T) {
	if !ClonesVolumes("hostpath.csi.k8s.io") || ClonesVolumes("ebs.csi.aws.com") || ClonesVolumes("") {
		t.Error("ClonesVolumes")
	}
}

func TestCloneObjectsRefuseAnIncompleteInput(t *testing.T) {
	src, r := cloneSource(t)
	in := cloneInputFor(r)
	same := in
	same.Name = src.StatefulSet.Name
	noSpec := src
	noSpec.SpecSecret = nil
	badGen := src
	badGen.Gen = 0
	for name, c := range map[string]struct {
		src CloneSource
		in  CloneInput
	}{"own name": {src, same}, "no spec secret": {noSpec, in}, "no generation": {badGen, in}} {
		if _, err := CloneObjects(c.src, c.in); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestCloneJobRunsBesideTheSourceAsItsPodDoes(t *testing.T) {
	src, r := cloneSource(t)
	in := cloneInputFor(r)
	small := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resourceQuantity(t, "100m")}}
	job, err := CloneJob(src.StatefulSet, in, 1, JobInput{
		Name: names.CopierJob(in.Name, 1), Node: "node-7", Resources: &small,
		Claims: []JobClaim{{ClaimName: src.Disk.Name, Path: "/fleet-src", ReadOnly: true}},
	}, names.RoleCopier)
	if err != nil {
		t.Fatal(err)
	}
	spec := job.Job.Spec.Template.Spec
	ts := src.StatefulSet.Spec.Template.Spec
	if spec.Containers[0].Image != ts.Containers[0].Image || job.Job.Labels[names.LabelRole] != string(names.RoleCopier) || job.Job.Labels[names.LabelKey] != in.Key {
		t.Fatalf("job image %s labels %v", spec.Containers[0].Image, job.Job.Labels)
	}
	if jsonString(spec.Tolerations) != jsonString(ts.Tolerations) || jsonString(spec.ImagePullSecrets) != jsonString(ts.ImagePullSecrets) || ptrString(spec.RuntimeClassName) != ptrString(ts.RuntimeClassName) {
		t.Error("the Job is not placed as the source's pod is")
	}
	req := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) != 1 || req.NodeSelectorTerms[0].MatchFields[0].Values[0] != "node-7" {
		t.Fatalf("not pinned to the source's node: %s", jsonString(spec.Affinity))
	}
	var disk, source bool
	for _, v := range spec.Volumes {
		if pvc := v.PersistentVolumeClaim; pvc != nil {
			disk = disk || pvc.ClaimName == in.Name
			source = source || (pvc.ClaimName == src.Disk.Name && pvc.ReadOnly)
		}
	}
	if !disk || !source {
		t.Fatalf("volumes %s", jsonString(spec.Volumes))
	}
	if spec.Containers[0].SecurityContext.Privileged != nil {
		t.Error("the copier runs privileged")
	}
	if got := CheckHardened(job.Job.Spec.Template.ObjectMeta, spec, in.renderer(1).hardenOptions()); len(got) > 0 {
		t.Errorf("the copier is not hardened: %v", got)
	}
}

func TestPinToNodeJoinsEveryTerm(t *testing.T) {
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}, {}},
	}}}}
	pinToNode(spec, "n1")
	for i, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchFields) != 1 || term.MatchFields[0].Values[0] != "n1" {
			t.Errorf("term %d: %s", i, jsonString(term))
		}
	}
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func resourceQuantity(t *testing.T, s string) resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestCloneSpecOfReadsTheTemplate(t *testing.T) {
	src, r := cloneSource(t)
	o, err := CloneObjects(src, cloneInputFor(r))
	if err != nil {
		t.Fatal(err)
	}
	s := CloneSpecOf(o)
	if s.Hostname != names.Pod(o.StatefulSet.Name) {
		t.Errorf("hostname %q", s.Hostname)
	}
	if len(s.Volumes) != 1 || s.Volumes[0].Target != "/var/lib/docker" || !s.Volumes[0].CopyUp {
		t.Errorf("volumes %+v", s.Volumes)
	}
	want := SeedSpec(&Result{}, o).MountDirs
	if strings.Join(s.MountDirs, ",") != strings.Join(want, ",") || len(want) == 0 {
		t.Errorf("mount dirs %v, want %v", s.MountDirs, want)
	}
}
