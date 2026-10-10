//go:build !nok8s

package podspec

import (
	"fmt"
	"path"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// fleet's Jobs (the seed, copier and tool Jobs) all share one bootstrap
// protocol (jobboot): the instance's own image starts a shell loop that
// waits for fleetd to stage the fleet binary and the job spec into an
// emptyDir, then runs it. The renderer gives each the instance's placement
// (node selector, tolerations, affinity, runtime class) and the instance's
// resource requests, so the node the Job binds the instance disk on can run
// the instance; its DNS configuration, so the resolv.conf the seed copies is
// the instance's; and the instance's security level, so the files it writes
// are labelled for the instance.

// JobInput is one Job of an instance.
type JobInput struct {
	// Name is the Job's name: names.SeedJob for the seed.
	Name string
	// ToolsDir is where the tools emptyDir mounts; "" is jobboot.ToolsDir.
	ToolsDir string
	// Command is the container's command; nil is the bootstrap protocol's
	// wait loop on ToolsDir (jobboot.Command).
	Command []string
	// DiskPath is where the instance disk mounts, whole: the job spec's PVC
	// path. "" is JobDiskPath.
	DiskPath string
	// Claims are other disks the Job mounts (the fleet share a first seed
	// binds beside the instance disk, a clone's source).
	Claims []JobClaim
	// ActiveDeadline bounds the Job (a seed's: jobboot.SeedDeadline); 0
	// leaves it unbounded.
	ActiveDeadline time.Duration
	// TTL keeps the finished Job, its pod's log and termination message for
	// fleetd to read; 0 is jobboot.TTLAfterFinished.
	TTL time.Duration
	// Identity runs the Job as someone else than the instance (a tool Job
	// reading the disk as remoteUser); nil is the instance's.
	Identity *Identity
	// Resources are the Job container's; nil is the instance's, which is
	// what a seed needs: the node it binds the instance disk on must fit
	// the instance. A Job that runs beside the instance (a copier) asks for
	// little instead.
	Resources *corev1.ResourceRequirements
}

// JobClaim is a disk a Job mounts besides the instance's.
type JobClaim struct {
	ClaimName string
	Path      string
	ReadOnly  bool
}

// JobObjects is a rendered Job.
type JobObjects struct {
	Job *batchv1.Job
}

// Pod is the pod the Job creates, as the API server first sees it (its
// name generated): for the dry-run.
func (j *JobObjects) Pod() *corev1.Pod {
	t := j.Job.Spec.Template.DeepCopy()
	p := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: t.ObjectMeta,
		Spec:       t.Spec,
	}
	p.GenerateName, p.Namespace = j.Job.Name+"-", j.Job.Namespace
	return p
}

// The Job pod's volumes.
const (
	volumeTools = "tools"
	volumeClaim = "claim"
)

// RenderJob renders a Job of the instance res translated (a copier or tool
// Job; Render renders the seed with RenderInput.Seed).
func RenderJob(res *Result, in RenderInput, job JobInput, role names.Role) (*JobObjects, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := &renderer{res: res, pod: &res.Pod, in: in, cluster: in.Cluster}
	if r.cluster == nil {
		r.cluster = &fleet.KubeCluster{}
	}
	r.parseQuantities()
	if err := joinRefusals(r.refusals); err != nil {
		return nil, err
	}
	return r.job(job, role)
}

func (r *renderer) job(in JobInput, role names.Role) (*JobObjects, error) {
	if in.ToolsDir == "" {
		in.ToolsDir = jobboot.ToolsDir
	}
	if in.DiskPath == "" {
		in.DiskPath = JobDiskPath
	}
	if len(in.Command) == 0 {
		in.Command = jobboot.Command(jobboot.Paths{Dir: in.ToolsDir}, 0)
	}
	if in.Name == "" || !path.IsAbs(in.ToolsDir) || !path.IsAbs(in.DiskPath) {
		return nil, fmt.Errorf("podspec: job input needs a name and absolute tools and disk paths")
	}
	if !names.IsDNS1123Label(in.Name) || !names.IsLabelValue(in.Name) {
		return nil, fmt.Errorf("podspec: job name %q is not a DNS label", in.Name)
	}
	if role == names.RoleInstance {
		return nil, fmt.Errorf("podspec: a Job's pod cannot take the instance role (the StatefulSet would adopt it)")
	}
	identity := r.in.Identity
	if in.Identity != nil {
		identity = *in.Identity
	}

	volumes := []corev1.Volume{
		{Name: volumeDisk, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.in.Name}}},
		{Name: volumeTools, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: volumeDisk, MountPath: in.DiskPath},
		{Name: volumeTools, MountPath: in.ToolsDir},
	}
	for i, c := range in.Claims {
		if c.ClaimName == "" || !path.IsAbs(c.Path) {
			return nil, fmt.Errorf("podspec: job claim %d needs a claim name and an absolute path", i)
		}
		name := volumeClaim + "-" + strconv.Itoa(i)
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.ClaimName, ReadOnly: c.ReadOnly}}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: c.Path, ReadOnly: c.ReadOnly})
	}
	seen := map[string]bool{}
	for _, m := range mounts {
		if seen[m.MountPath] {
			return nil, fmt.Errorf("podspec: two mounts at %s in job %s", m.MountPath, in.Name)
		}
		seen[m.MountPath] = true
	}

	resources := r.resources()
	if in.Resources != nil {
		resources = *in.Resources.DeepCopy()
	}
	spec := r.podSpec(identity)
	spec.Volumes = volumes
	spec.RestartPolicy = corev1.RestartPolicyNever
	spec.TerminationGracePeriodSeconds = ptr(int64(DefaultStopTimeout))
	spec.Containers = []corev1.Container{{
		Name:                     JobContainer,
		Image:                    r.in.Image.Ref,
		ImagePullPolicy:          r.pullPolicy(),
		Command:                  append([]string(nil), in.Command...),
		Resources:                resources,
		VolumeMounts:             mounts,
		SecurityContext:          r.containerSecurity(Security{}, identity),
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}}
	labels := r.labels(role)
	annotations := r.podAnnotations(JobContainer)
	// A Job ends on its own; nothing keeps the cluster from scaling its node
	// down once it has.
	delete(annotations, annotationSafeToEvict)
	t := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: spec}
	opts := r.hardenOptions()
	opts.AllowHostAccess = false
	if err := HardenTemplate(&t, opts); err != nil {
		return nil, err
	}

	ttl := in.TTL
	if ttl == 0 {
		ttl = jobboot.TTLAfterFinished
	}
	meta := r.meta(in.Name, role)
	meta.Annotations = map[string]string{names.AnnotationRebuildGen: strconv.Itoa(r.in.Gen)}
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: meta,
		Spec: batchv1.JobSpec{
			// fleetd decides what happens after a failure; the Job never
			// retries.
			BackoffLimit:            ptr(int32(jobboot.BackoffLimit)),
			Completions:             ptr(int32(1)),
			Parallelism:             ptr(int32(1)),
			TTLSecondsAfterFinished: ptr(int32(ttl / time.Second)),
			Template:                t,
		},
	}
	if in.ActiveDeadline > 0 {
		job.Spec.ActiveDeadlineSeconds = ptr(int64((in.ActiveDeadline + time.Second - 1) / time.Second))
	}
	return &JobObjects{Job: job}, nil
}
