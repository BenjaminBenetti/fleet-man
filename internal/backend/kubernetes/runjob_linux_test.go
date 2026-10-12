//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The tests below drive Jobs whose pods are local: an exec into a Job pod
// runs the staging scripts against a local tools dir, and `pod-job feed`
// reaches an in-process stand-in of pod-job over the real jobboot socket,
// so the staging, the spec and the feed are the real ones.

const jobNS = "fleet-test"

type jobHarness struct {
	t     *testing.T
	cs    *fake.Clientset
	pool  *Pool
	tools string
	store *binstore.Store

	mu        sync.Mutex
	execs     []string
	breakFeed int
	// result is what the stand-in pod-job ends with; specs the job.json
	// each run was handed.
	result jobboot.Result
	specs  []podseed.JobSpec
	// podFor makes the pod of a new Job (nil: a running one; returning nil:
	// none).
	podFor    func(job *batchv1.Job) *corev1.Pod
	uids      int
	stagedPod string
}

func newJobHarness(t *testing.T) *jobHarness {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the fake pod binary")
	}
	tools, err := os.MkdirTemp("/tmp", "rj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tools) })
	h := &jobHarness{t: t, cs: fake.NewClientset(), tools: tools, result: jobboot.Result{OK: true, Output: []byte(`{"gen":2}`)}}
	// The API server gives every object it creates a UID, the fake one
	// does not for a Job created by an apply.
	h.cs.PrependReactor("patch", "jobs", func(a k8stesting.Action) (bool, kruntime.Object, error) {
		handled, obj, err := k8stesting.ObjectReaction(h.cs.Tracker())(a)
		if job, ok := obj.(*batchv1.Job); ok && err == nil && job.UID == "" {
			h.mu.Lock()
			h.uids++
			job.UID = types.UID(fmt.Sprintf("uid-%s-%d", job.Name, h.uids))
			h.mu.Unlock()
			if err := h.cs.Tracker().Update(batchv1.SchemeGroupVersion.WithResource("jobs"), job, job.Namespace); err != nil {
				return true, nil, err
			}
		}
		return handled, obj, err
	})
	bin := fakePod(t, "vtest-1")
	h.store = &binstore.Store{Dir: t.TempDir(), Self: binstore.Identity{Version: "vtest-1"}, Getenv: func(k string) string {
		if k == binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH) {
			return bin
		}
		return ""
	}}
	kubeconfig := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, kubeconfig, "tok")
	rec := fleet.KubeCluster{ID: testCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: jobNS}
	h.pool = NewPool(PoolOptions{
		Records:   func() ([]fleet.KubeCluster, error) { return []fleet.KubeCluster{rec}, nil },
		Environ:   func() []string { return []string{"PATH=/usr/bin:/bin"} },
		Bins:      h.store,
		Exec:      h.exec,
		Clientset: func(*Cluster) (kclient.Interface, error) { return h.cs, nil },
		Owner:     func() (string, error) { return testOwner, nil },
		Version:   "vtest-1",
	})
	t.Cleanup(h.pool.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.makePods(ctx)
	go h.podJobs(ctx)
	return h
}

// makePods plays the Job controller: every new Job gets its pod.
func (h *jobHarness) makePods(ctx context.Context) {
	made := map[types.UID]bool{}
	for ctx.Err() == nil {
		jobs, _ := h.cs.BatchV1().Jobs(jobNS).List(ctx, metav1.ListOptions{})
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if made[job.UID] || job.UID == "" {
				continue
			}
			made[job.UID] = true
			h.mu.Lock()
			podFor := h.podFor
			h.mu.Unlock()
			var pod *corev1.Pod
			if podFor != nil {
				pod = podFor(job)
			} else {
				pod = jobPodOf(job, corev1.PodRunning)
			}
			if pod != nil {
				// A pod name of its own, as the controller's random suffix
				// gives a Job made again.
				pod.Name = fmt.Sprintf("%s-p%d", job.Name, len(made))
				_, _ = h.cs.CoreV1().Pods(jobNS).Create(ctx, pod, metav1.CreateOptions{})
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// jobPodOf is a pod the Job controller made for job, in phase.
func jobPodOf(job *batchv1.Job, phase corev1.PodPhase) *corev1.Pod {
	yes := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-x1y2z", Namespace: jobNS, UID: types.UID("pod-" + string(job.UID)),
			Labels:            map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)},
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &yes}},
			CreationTimestamp: metav1.Now(),
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: podspec.JobContainer}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	if phase == corev1.PodRunning {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	}
	return pod
}

// podJobs stands in for `fleet pod-job` in the pod staged into: for each
// staged spec, it checks the binary is the one the spec names, serves the
// socket until fleetd collected its result, and leaves its termination
// message on the pod.
func (h *jobHarness) podJobs(ctx context.Context) {
	ready := filepath.Join(h.tools, jobboot.ReadyName)
	for ctx.Err() == nil {
		if _, err := os.Stat(ready); err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		_ = os.Remove(ready)
		h.mu.Lock()
		pod := h.stagedPod
		h.mu.Unlock()
		h.podJob(ctx, pod)
	}
}

func (h *jobHarness) podJob(ctx context.Context, pod string) {
	spec, err := podseed.LoadJobSpec(filepath.Join(h.tools, jobboot.SpecName))
	h.mu.Lock()
	h.specs = append(h.specs, spec)
	result := h.result
	h.mu.Unlock()
	if err != nil {
		result = jobboot.Result{Error: err.Error()}
	} else if sha, err := podseed.FileSHA256(filepath.Join(h.tools, jobboot.BinaryName)); err != nil || sha != spec.BinarySHA256 {
		result = jobboot.Result{Error: fmt.Sprintf("binary sha %s, spec %s (%v)", sha, spec.BinarySHA256, err)}
	}
	srv, err := jobboot.Listen(filepath.Join(h.tools, jobboot.SocketName), jobboot.ServerOptions{Op: string(spec.Op), Gen: spec.Gen})
	if err != nil {
		h.t.Errorf("pod-job listen: %v", err)
		return
	}
	defer srv.Close()
	_ = srv.Finish(ctx, result, 300*time.Millisecond)
	term := jobboot.Termination{OK: result.OK, Bytes: result.Bytes, Error: result.Error}
	code := int32(0)
	if !result.OK {
		code = jobboot.ExitFailed
	}
	if p, err := h.cs.CoreV1().Pods(jobNS).Get(ctx, pod, metav1.GetOptions{}); err == nil {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: string(term.Encode())}}}}
		_, _ = h.cs.CoreV1().Pods(jobNS).UpdateStatus(ctx, p, metav1.UpdateOptions{})
	}
}

func (h *jobHarness) exec(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
	if req.Namespace != jobNS || req.Container != podspec.JobContainer {
		return fmt.Errorf("exec into %s/%s container %q", req.Namespace, req.Pod, req.Container)
	}
	argv := req.Command
	switch {
	case len(argv) == 3 && argv[0] == "sh" && argv[2] == jobboot.ProbeScript:
		h.note("probe")
	case len(argv) == 3 && argv[0] == "sh":
		h.note("stage")
		h.mu.Lock()
		h.stagedPod = req.Pod
		h.mu.Unlock()
	case len(argv) >= 3 && argv[1] == "pod-job" && argv[2] == "feed":
		h.note("feed")
		h.mu.Lock()
		broken := h.breakFeed > 0
		if broken {
			h.breakFeed--
		}
		h.mu.Unlock()
		if broken {
			return errors.New("connection reset by peer")
		}
		socket := strings.ReplaceAll(argv[len(argv)-1], jobboot.ToolsDir, h.tools)
		return jobboot.RunPipe(ctx, socket, stdio.Stdin, stdio.Stdout, 5*time.Second)
	default:
		return fmt.Errorf("unexpected exec %q", argv)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", strings.ReplaceAll(argv[2], jobboot.ToolsDir, h.tools))
	cmd.Stdout, cmd.Stderr = stdio.Stdout, stdio.Stderr
	if req.Stdin && stdio.Stdin != nil {
		cmd.Stdin = stdio.Stdin
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &kubeexec.ExitError{Code: exitErr.ExitCode()}
	}
	return err
}

func (h *jobHarness) note(kind string) {
	h.mu.Lock()
	h.execs = append(h.execs, kind)
	h.mu.Unlock()
}

func (h *jobHarness) execKinds() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.execs, ",")
}

// addJob creates a Job fleet made.
func (h *jobHarness) addJob(name string, mutate func(*batchv1.Job)) *batchv1.Job {
	h.t.Helper()
	labels := names.Base(testOwner, testCluster)
	labels[names.LabelRole] = string(names.RoleTool)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: jobNS, Labels: labels, UID: types.UID("uid-" + name)}}
	if mutate != nil {
		mutate(job)
	}
	got, err := h.cs.BatchV1().Jobs(jobNS).Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

func rollbackSpec(gen int) podseed.JobSpec {
	return podseed.JobSpec{Op: podseed.OpRollback, Gen: gen, PVC: podspec.JobDiskPath}
}

func fastDeadlines() *Deadlines {
	d := DefaultDeadlines()
	return &d
}

// A Job runs as every fleet Job does: probe, the binary staged, the spec
// (naming that binary) staged last, then the feed to its result.
func TestRunJob(t *testing.T) {
	h := newJobHarness(t)
	h.addJob("fi-a-one-aaaaaaaa-rollback-g2", nil)
	res, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "fi-a-one-aaaaaaaa-rollback-g2", Spec: rollbackSpec(2), Interval: 5 * time.Millisecond, Deadlines: fastDeadlines()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || string(res.Output) != `{"gen":2}` {
		t.Fatalf("result %+v", res)
	}
	if got := h.execKinds(); got != "probe,stage,stage,feed" {
		t.Fatalf("execs %s", got)
	}
	art, _ := h.store.Binary(context.Background(), runtime.GOARCH)
	h.mu.Lock()
	spec := h.specs[0]
	h.mu.Unlock()
	if spec.BinarySHA256 != art.SHA256 || spec.Op != podseed.OpRollback || spec.Gen != 2 || spec.V != podseed.JobSpecVersion {
		t.Fatalf("staged spec %+v (binary %s)", spec, art.SHA256)
	}
	if fi, err := os.Stat(filepath.Join(h.tools, jobboot.BinaryName)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged binary %v %v", fi, err)
	}
}

// A job that ran and failed is a result, not an error.
func TestRunJobThatFails(t *testing.T) {
	h := newJobHarness(t)
	h.result = jobboot.Result{Error: "restore generation 2's root: no space left on device"}
	h.addJob("j-fails", nil)
	res, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "j-fails", Spec: rollbackSpec(2), Interval: 5 * time.Millisecond})
	if err != nil || res.OK || !strings.Contains(res.Error, "no space left") {
		t.Fatalf("result %+v, err %v", res, err)
	}
}

// A feed that breaks before the result falls back to the Job's end and its
// termination message.
func TestRunJobFallsBackToTheTerminationMessage(t *testing.T) {
	h := newJobHarness(t)
	h.breakFeed = 1
	h.addJob("j-broken-feed", nil)
	res, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "j-broken-feed", Spec: rollbackSpec(2), Interval: 5 * time.Millisecond})
	if err != nil || !res.OK {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := h.execKinds(); got != "probe,stage,stage,feed" {
		t.Fatalf("execs %s", got)
	}
}

func TestRunJobFailures(t *testing.T) {
	short := func() *Deadlines {
		d := DefaultDeadlines()
		d.PodReady, d.ImagePull, d.Unschedulable = 60*time.Millisecond, 60*time.Millisecond, 60*time.Millisecond
		return &d
	}
	deadline := int64(900)
	for _, tc := range []struct {
		name   string
		job    func(*batchv1.Job)
		pod    func(*batchv1.Job) *corev1.Pod
		code   string
		substr string
	}{
		{name: "deadline", job: func(j *batchv1.Job) {
			j.Spec.ActiveDeadlineSeconds = &deadline
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"}}
		}, code: CodeTimeout, substr: "after 15m0s"},
		{name: "image does not pull", pod: func(j *batchv1.Job) *corev1.Pod {
			p := jobPodOf(j, corev1.PodPending)
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}}}
			return p
		}, code: CodeImagePull, substr: "ImagePullBackOff"},
		{name: "not schedulable", pod: func(j *batchv1.Job) *corev1.Pod {
			p := jobPodOf(j, corev1.PodPending)
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes are available: 1 Insufficient memory."}}
			return p
		}, code: CodeUnschedulable, substr: "Insufficient memory"},
		{name: "a pod the Job does not control", pod: func(j *batchv1.Job) *corev1.Pod {
			p := jobPodOf(j, corev1.PodRunning)
			p.OwnerReferences[0].UID = "someone-else"
			return p
		}, code: CodeTimeout, substr: "no pod was created"},
		{name: "pod ended before staging", pod: func(j *batchv1.Job) *corev1.Pod {
			p := jobPodOf(j, corev1.PodFailed)
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: jobboot.ExitNotStaged, Reason: "Error"}}}}
			return p
		}, substr: "exit code 70"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t)
			if tc.pod != nil {
				h.podFor = tc.pod
			}
			h.addJob("j", tc.job)
			_, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "j", Spec: rollbackSpec(2), Interval: 5 * time.Millisecond, Deadlines: short()})
			var coded interface{ Code() string }
			if err == nil || !strings.Contains(err.Error(), tc.substr) || (tc.code != "" && (!errors.As(err, &coded) || coded.Code() != tc.code)) {
				t.Fatalf("err = %v, want code %q containing %q", err, tc.code, tc.substr)
			}
			if got := h.execKinds(); got != "" {
				t.Fatalf("a Job that never ran got execs: %s", got)
			}
		})
	}
}

func TestRunJobRefusesAJobFleetDidNotMake(t *testing.T) {
	h := newJobHarness(t)
	h.addJob("foreign", func(j *batchv1.Job) { j.Labels = map[string]string{"app": "x"} })
	if _, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "foreign", Spec: rollbackSpec(2)}); err == nil || !strings.Contains(err.Error(), "not one fleet made") {
		t.Fatalf("err = %v", err)
	}
}

// ApplyJob creates the Job from its apply body under fleet's field manager.
func TestApplyJob(t *testing.T) {
	h := newJobHarness(t)
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": "applied", "labels": names.Base(testOwner, testCluster)},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": "job", "image": "x"}}}}},
	})
	job, err := h.pool.ApplyJob(context.Background(), testCluster, body)
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "applied" || job.Labels[names.LabelManagedBy] != names.ManagedBy {
		t.Fatalf("applied %+v", job.ObjectMeta)
	}
	if _, err := h.pool.ApplyJob(context.Background(), testCluster, []byte(`{"kind":"Job"}`)); err == nil {
		t.Fatal("a nameless Job applied")
	}
}

// A Job's pod that waits before it runs (an image its node lost) is heard
// by Waiting each time RunJob looks, so the image can be delivered again;
// once the pod runs, the Job runs as any other.
func TestRunJobHearsAPodThatWaits(t *testing.T) {
	h := newJobHarness(t)
	h.podFor = func(job *batchv1.Job) *corev1.Pod {
		pod := jobPodOf(job, corev1.PodPending)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ErrImageNeverPull", Message: `Container image "fleet.invalid/d/x:1" is not present with pull policy of Never`}}}}
		return pod
	}
	h.addJob("j-waits", nil)
	var mu sync.Mutex
	heard := 0
	waiting := func(ctx context.Context, pod *corev1.Pod) {
		st := containerStatus(pod, podspec.JobContainer)
		if st == nil || st.State.Waiting == nil || st.State.Waiting.Reason != "ErrImageNeverPull" {
			return
		}
		mu.Lock()
		heard++
		first := heard == 1
		mu.Unlock()
		if first {
			// The image is back: the kubelet starts the container.
			pod = pod.DeepCopy()
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: podspec.JobContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			if _, err := h.cs.CoreV1().Pods(jobNS).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
		}
	}
	res, err := h.pool.RunJob(context.Background(), JobRun{Cluster: testCluster, Job: "j-waits", Spec: rollbackSpec(2), Interval: 5 * time.Millisecond, Deadlines: fastDeadlines(), Waiting: waiting})
	if err != nil || !res.OK {
		t.Fatalf("result %+v, err %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if heard == 0 {
		t.Fatal("Waiting never heard the pod that waited on its image")
	}
}
