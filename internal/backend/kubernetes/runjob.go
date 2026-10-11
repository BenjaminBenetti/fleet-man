//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// runjob.go drives a jobboot Job (internal/jobboot) from fleetd's side, the
// way every fleet Job runs (seed, rollback, copier, tool): wait for the
// Job's pod to run, with the deadlines; probe it; stage the pod build of
// fleet and the job's spec into its tools dir (the spec last, with the
// sentinel that releases the pod's wait loop); then feed it over a kube exec
// of `fleet pod-job feed` (the workspace, when the job takes one) until its
// result. A transport that breaks before the result falls back to the
// Job's own end and its termination message.

// JobRun is one Job RunJob drives.
type JobRun struct {
	Cluster string
	// Job is the Job's name: it exists already (ApplyJob), its UID
	// recorded in the caller's journal.
	Job string
	// Spec is the job.json it runs; RunJob fills in BinarySHA256, the
	// binary it stages.
	Spec podseed.JobSpec
	// Workspace opens the workspace tar for a job that takes one; it is
	// opened again if the job refused a transfer. Nil sends none.
	Workspace func() (io.ReadCloser, error)
	// Progress receives the job's progress; nil drops it.
	Progress func(jobboot.Progress)
	// PVC is the disk the Job mounts, watched for binding (a seed binds
	// the instance's disk); "" watches none.
	PVC string
	// Deadlines bound the wait for the Job's pod; nil is DefaultDeadlines().
	Deadlines *Deadlines
	// Interval is the time between looks at the Job (DefaultWaitInterval).
	Interval time.Duration
	// Check, when set, verifies the Job's pod once it runs, before anything
	// is staged into it or fed to it (the workspace may hold secrets): its
	// error ends the run.
	Check func(*corev1.Pod) error
	// While, when set, runs once the Job's pod has fleet's binary and the
	// spec, before the job's result is collected: what fleetd does in the
	// pod beside the job (a tool Job's workspace export), with exec running
	// a command in the Job's container. The job is collected all the same,
	// so its pod ends; While's error is then RunJob's.
	While func(ctx context.Context, exec JobExec) error
	// Pending, when set, is asked each time RunJob looks at a Job whose pod
	// does not run yet: its error ends the wait (a disk the provisioner
	// refused, which no deadline need be waited out for).
	Pending func(context.Context) error
	// Scheduling, when set, hears each new reason the scheduler gives for
	// keeping the Job's pod unscheduled ("Unschedulable: ...").
	Scheduling func(ctx context.Context, reason string)
	// Barrier does what the job waits for at a barrier (jobboot): a clone's
	// copier holding its source still for the final delta. nil refuses
	// every barrier.
	Barrier func(context.Context, jobboot.Barrier) error
}

// JobExec runs argv in a Job's container (as its user, in its own exec),
// its output to stdout and stderr; a non-zero exit is an error.
type JobExec func(ctx context.Context, argv []string, stdout, stderr io.Writer) error

// ApplyJob creates (or updates) a Job from its server-side apply body
// (podspec.ApplyBody of a podspec.RenderJob), under fleet's field manager.
func (p *Pool) ApplyJob(ctx context.Context, clusterID string, body []byte) (*batchv1.Job, error) {
	c, err := p.Cluster(clusterID)
	if err != nil {
		return nil, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, err
	}
	return applyJob(ctx, cs, c.Namespace(), body)
}

func applyJob(ctx context.Context, cs kclient.Interface, ns string, body []byte) (*batchv1.Job, error) {
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &meta); err != nil || meta.Metadata.Name == "" {
		return nil, fmt.Errorf("a Job apply body without a name: %v", err)
	}
	force := true
	job, err := cs.BatchV1().Jobs(ns).Patch(ctx, meta.Metadata.Name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: DefaultFieldManager, Force: &force})
	if err != nil {
		return nil, fmt.Errorf("applying Job %s/%s: %w", ns, meta.Metadata.Name, err)
	}
	return job, nil
}

// RunJob drives Job r.Job to its end and returns its result: a job that ran
// and failed is a Result with OK false and a nil error; the error is for the
// Job that never ran, never finished or could not be reached.
func (p *Pool) RunJob(ctx context.Context, r JobRun) (jobboot.Result, error) {
	c, err := p.Cluster(r.Cluster)
	if err != nil {
		return jobboot.Result{}, err
	}
	cs, err := p.clientset(c)
	if err != nil {
		return jobboot.Result{}, err
	}
	ns := c.Namespace()
	job, err := cs.BatchV1().Jobs(ns).Get(ctx, r.Job, metav1.GetOptions{})
	if err != nil {
		return jobboot.Result{}, fmt.Errorf("reading Job %s/%s: %w", ns, r.Job, err)
	}
	if job.Labels[names.LabelManagedBy] != names.ManagedBy {
		return jobboot.Result{}, fmt.Errorf("job %s/%s is not one fleet made", ns, r.Job)
	}
	d := DefaultDeadlines()
	if r.Deadlines != nil {
		d = *r.Deadlines
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultWaitInterval
	}
	pod, err := p.waitJobPod(ctx, cs, ns, job, r, d, interval)
	if err != nil {
		return jobboot.Result{}, err
	}
	if r.Check != nil {
		if err := r.Check(pod); err != nil {
			p.opts.Logger.Error("kube job: refused a Job pod that is not the one fleet made", "cluster", r.Cluster, "job", ns+"/"+r.Job, "err", err)
			return jobboot.Result{}, err
		}
	}

	paths := jobboot.Paths{Dir: r.Spec.Tools}
	var probeOut, probeErr bytes.Buffer
	probeReq := kubeexec.ExecRequest{Namespace: ns, Pod: pod.Name, Container: podspec.JobContainer, Command: []string{"sh", "-c", jobboot.ProbeScript}}
	if err := p.exec(ctx, c, probeReq, kubeexec.Stdio{Stdout: &limitedWriter{w: &probeOut, n: 64 << 10}, Stderr: &limitedWriter{w: &probeErr, n: 4 << 10}}); err != nil {
		return jobboot.Result{}, fmt.Errorf("probing Job pod %s/%s: %w%s", ns, pod.Name, err, detail(probeErr.String()))
	}
	probe, err := jobboot.ParseProbe(probeOut.String())
	if err != nil {
		return jobboot.Result{}, fmt.Errorf("probing Job pod %s/%s: %w", ns, pod.Name, err)
	}
	if !probe.Head {
		return jobboot.Result{}, errors.New("the image has no head(1), which fleet stages its binary with")
	}
	art, err := p.bins().Binary(ctx, probe.Arch)
	if err != nil {
		return jobboot.Result{}, err
	}
	staged, wire, err := p.bins().Staged(ctx, art, probe.Gzip)
	if err != nil {
		return jobboot.Result{}, err
	}
	f, err := os.Open(staged)
	if err != nil {
		return jobboot.Result{}, err
	}
	err = p.stageJobFile(ctx, c, ns, pod.Name, jobboot.StageFile{Path: paths.Binary(), Size: art.Size, SHA256: art.SHA256, Gzip: probe.Gzip, WireSize: wire, Mode: 0o755, CheckSHA: probe.SHA256Sum}, f)
	_ = f.Close()
	if err != nil {
		return jobboot.Result{}, err
	}
	spec := r.Spec
	if spec.V == 0 {
		spec.V = podseed.JobSpecVersion
	}
	spec.BinarySHA256 = art.SHA256
	data, err := json.Marshal(spec)
	if err != nil {
		return jobboot.Result{}, err
	}
	sum := sha256.Sum256(data)
	if err := p.stageJobFile(ctx, c, ns, pod.Name, jobboot.StageFile{Path: paths.Spec(), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), CheckSHA: probe.SHA256Sum, Ready: true, ReadyPath: paths.Ready()}, bytes.NewReader(data)); err != nil {
		return jobboot.Result{}, err
	}
	p.opts.Logger.Info("kube job: staged", "cluster", r.Cluster, "job", ns+"/"+r.Job, "op", spec.Op, "gen", spec.Gen, "fleet_sha", art.SHA256[:12])

	var whileErr error
	if r.While != nil {
		whileErr = r.While(ctx, func(ctx context.Context, argv []string, stdout, stderr io.Writer) error {
			req := kubeexec.ExecRequest{Namespace: ns, Pod: pod.Name, Container: podspec.JobContainer, Command: argv}
			return p.exec(ctx, c, req, kubeexec.Stdio{Stdout: stdout, Stderr: stderr})
		})
	}
	res, err := p.feedJob(ctx, c, ns, pod.Name, paths, r)
	if whileErr != nil {
		return res, whileErr
	}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return jobboot.Result{}, ctx.Err()
	}
	// The feed broke before the result: the Job still ends on its own, and
	// says how in its termination message.
	p.opts.Logger.Warn("kube job: feed broke; waiting for the Job to end", "cluster", r.Cluster, "job", ns+"/"+r.Job, "err", err)
	end, endErr := p.waitJobEnd(ctx, cs, ns, job, pod.Name, d, interval)
	if endErr != nil {
		return jobboot.Result{}, fmt.Errorf("%w (and the feed broke: %v)", endErr, err)
	}
	return end, nil
}

// stageJobFile writes one file into the Job pod's tools dir over its own
// exec.
func (p *Pool) stageJobFile(ctx context.Context, c *Cluster, ns, pod string, f jobboot.StageFile, content io.Reader) error {
	script, err := jobboot.StageScript(f)
	if err != nil {
		return err
	}
	var errOut bytes.Buffer
	req := kubeexec.ExecRequest{Namespace: ns, Pod: pod, Container: podspec.JobContainer, Command: []string{"sh", "-c", script}, Stdin: true}
	if err := p.exec(ctx, c, req, kubeexec.Stdio{Stdin: content, Stdout: io.Discard, Stderr: &limitedWriter{w: &errOut, n: 4 << 10}}); err != nil {
		return fmt.Errorf("staging %s into Job pod %s/%s: %w%s", f.Path, ns, pod, err, detail(errOut.String()))
	}
	return nil
}

// feedDrain bounds the wait for a fed job's exec to end after its result.
const feedDrain = 10 * time.Second

// feedJob runs the job's feed to its result, again on a new connection
// once if the job refused the workspace.
func (p *Pool) feedJob(ctx context.Context, c *Cluster, ns, pod string, paths jobboot.Paths, r JobRun) (jobboot.Result, error) {
	for attempt := 0; ; attempt++ {
		req := kubeexec.ExecRequest{Namespace: ns, Pod: pod, Container: podspec.JobContainer, Command: []string{paths.Binary(), "pod-job", "feed", "--socket", paths.Socket()}}
		stream := kubeexec.StartStream(ctx, req, func(ctx context.Context, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
			return p.exec(ctx, c, req, stdio)
		})
		opts := jobboot.FeedOptions{Progress: r.Progress, Barrier: r.Barrier}
		var ws io.ReadCloser
		if r.Workspace != nil {
			var err error
			if ws, err = r.Workspace(); err != nil {
				_ = stream.Close()
				return jobboot.Result{}, fmt.Errorf("opening the workspace: %w", err)
			}
			opts.Workspace = ws
		}
		res, err := jobboot.Feed(ctx, stream, opts)
		if ws != nil {
			_ = ws.Close()
		}
		if err == nil {
			// The job exits once its acknowledgement arrives: let it reach
			// the job (stdin closed, not the exec cut) before the exec goes.
			_ = stream.CloseWrite()
			select {
			case <-stream.Done():
			case <-time.After(feedDrain):
			case <-ctx.Done():
			}
		}
		_ = stream.Close()
		if errors.Is(err, jobboot.ErrWorkspaceRefused) && attempt == 0 {
			continue
		}
		if err != nil {
			if s := strings.TrimSpace(stream.Stderr()); s != "" {
				err = fmt.Errorf("%w%s", err, detail(s))
			}
			return jobboot.Result{}, err
		}
		return res, nil
	}
}

// jobFailed is a Job's Failed condition, if it has one.
func jobFailed(job *batchv1.Job) *batchv1.JobCondition {
	for i, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return &job.Status.Conditions[i]
		}
	}
	return nil
}

// jobFailure is the error of a Job the controller gave up.
func jobFailure(job *batchv1.Job, c *batchv1.JobCondition) error {
	if c.Reason == "DeadlineExceeded" {
		var after time.Duration
		if job.Spec.ActiveDeadlineSeconds != nil {
			after = time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second
		}
		return &DeadlineError{Kind: CodeTimeout, Wait: "Job " + job.Name + " to finish", After: after, Detail: joinReason(c.Reason, c.Message), Env: EnvSeedTimeout}
	}
	return fmt.Errorf("job %s failed: %s", job.Name, joinReason(c.Reason, c.Message))
}

// jobPod is the Job's pod that its controller made (the newest, when a
// failed one was replaced).
func jobPod(ctx context.Context, cs kclient.Interface, ns string, job *batchv1.Job) (*corev1.Pod, error) {
	list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + string(job.UID)})
	if err != nil {
		return nil, err
	}
	var best *corev1.Pod
	for i := range list.Items {
		pod := &list.Items[i]
		ref := metav1.GetControllerOf(pod)
		if ref == nil || ref.UID != job.UID || pod.DeletionTimestamp != nil {
			continue
		}
		if best == nil || best.CreationTimestamp.Before(&pod.CreationTimestamp) {
			best = pod
		}
	}
	return best, nil
}

// waitJobPod waits for the Job's container to run.
func (p *Pool) waitJobPod(ctx context.Context, cs kclient.Interface, ns string, job *batchv1.Job, r JobRun, d Deadlines, interval time.Duration) (*corev1.Pod, error) {
	w := newPodWait(d, podspec.JobContainer, "Job "+job.Name+"'s pod")
	lastScheduling := ""
	for {
		cur, err := cs.BatchV1().Jobs(ns).Get(ctx, job.Name, metav1.GetOptions{})
		switch {
		case gone(err):
			return nil, fmt.Errorf("job %s/%s was deleted", ns, job.Name)
		case err != nil:
			return nil, fmt.Errorf("reading Job %s/%s: %w", ns, job.Name, err)
		case cur.UID != job.UID:
			return nil, fmt.Errorf("job %s/%s was made again by someone else", ns, job.Name)
		}
		if c := jobFailed(cur); c != nil {
			return nil, jobFailure(cur, c)
		}
		pod, err := jobPod(ctx, cs, ns, cur)
		if err != nil {
			return nil, fmt.Errorf("listing Job %s/%s's pods: %w", ns, job.Name, err)
		}
		if pod != nil {
			if st := containerStatus(pod, podspec.JobContainer); st != nil && st.State.Running != nil && pod.Status.Phase == corev1.PodRunning {
				return pod, nil
			}
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return nil, fmt.Errorf("job %s/%s's pod ended before fleet staged it: %s", ns, job.Name, podEnd(pod))
			}
		}
		var pvc *corev1.PersistentVolumeClaim
		if r.PVC != "" {
			got, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, r.PVC, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("reading PVC %s/%s: %w", ns, r.PVC, err)
			}
			pvc = got
		}
		if err := w.observe(p.opts.Now(), pod, pvc); err != nil {
			return nil, explainMount(ctx, cs, pod, err)
		}
		if r.Scheduling != nil && pod != nil {
			reason := ""
			if c := unschedulable(pod); c != nil {
				reason = joinReason(c.Reason, c.Message)
			}
			if reason != lastScheduling {
				lastScheduling = reason
				if reason != "" {
					r.Scheduling(ctx, reason)
				}
			}
		}
		if r.Pending != nil {
			if err := r.Pending(ctx); err != nil {
				return nil, err
			}
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// waitJobEnd waits for the Job's pod to end and reads its termination
// message into a Result.
func (p *Pool) waitJobEnd(ctx context.Context, cs kclient.Interface, ns string, job *batchv1.Job, podName string, d Deadlines, interval time.Duration) (jobboot.Result, error) {
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return jobboot.Result{}, fmt.Errorf("reading Job pod %s/%s: %w", ns, podName, err)
		}
		if st := containerStatus(pod, podspec.JobContainer); st != nil && st.State.Terminated != nil {
			t := st.State.Terminated
			term, err := jobboot.ParseTermination(t.Message)
			if err != nil {
				return jobboot.Result{}, fmt.Errorf("job %s ended (exit code %d) without a result: %w", job.Name, t.ExitCode, err)
			}
			return jobboot.Result{OK: term.OK && t.ExitCode == 0, Bytes: term.Bytes, Error: term.Error}, nil
		}
		cur, err := cs.BatchV1().Jobs(ns).Get(ctx, job.Name, metav1.GetOptions{})
		if err == nil {
			if c := jobFailed(cur); c != nil {
				return jobboot.Result{}, jobFailure(cur, c)
			}
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return jobboot.Result{}, err
		}
	}
}

// podEnd says how a pod ended.
func podEnd(pod *corev1.Pod) string {
	if st := containerStatus(pod, podspec.JobContainer); st != nil && st.State.Terminated != nil {
		t := st.State.Terminated
		msg := strings.TrimSpace(t.Message)
		if term, err := jobboot.ParseTermination(msg); err == nil && term.Error != "" {
			msg = term.Error
		}
		return fmt.Sprintf("exit code %d (%s)", t.ExitCode, joinReason(t.Reason, msg))
	}
	if pod.Status.Reason != "" {
		return joinReason(pod.Status.Reason, pod.Status.Message)
	}
	return string(pod.Status.Phase)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
