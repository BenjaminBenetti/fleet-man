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
	"maps"
	"os"
	"path"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// rebuild_export.go is the first part of a rebuild: the workspace brought
// back to the daemon host's checkout from wherever the instance is, and the
// instance disk's room for the new root.
//
// A running pod sends its workspace folder over its bulk session (`fleet
// pod-job ws-export` as root, which reads every file a docker bind mount
// would show; nested mounts such as a node_modules volume come as empty
// dirs, as on the host). A stopped one's disk is read by a tool Job of its
// generation (podseed's OpTool), which holds the disk while the same export
// runs in its pod and reports the disk's room. The tar arrives into a
// directory beside the checkout, is checked against the summary the export
// prints, and only then replaces the checkout: a failed export leaves the
// checkout as it was. Owners on the host are the daemon user's.
//
// Symlinks come back as they are, pointing wherever they point, as the
// checkout a docker instance writes through its bind mount has them.

// exportMode is rebuild_export as the rebuild reads it.
func exportMode(v string) string {
	if strings.TrimSpace(v) == RebuildExportOff {
		return RebuildExportOff
	}
	return RebuildExportAuto
}

// exportArgv is the export command of fleet binary bin, of workspace dir.
func exportArgv(bin, dir string) []string {
	return []string{bin, "pod-job", "ws-export", "--dir", dir}
}

// export brings the workspace back (unless rebuild_export is off) and, for
// a stopped instance, measures its disk with the same tool Job.
func (rb *rebuilding) export(ctx context.Context) error {
	mode := exportMode(rb.rec.RebuildExport)
	if v := strings.TrimSpace(rb.rec.RebuildExport); v != "" && v != RebuildExportAuto && v != RebuildExportOff {
		rb.warn(ctx, fmt.Sprintf("rebuild_export %q is not auto or off; the workspace is exported", v))
	}
	detail := "from the running pod"
	if !rb.running() {
		detail = "from the stopped instance's disk"
	}
	if mode == RebuildExportOff {
		detail = "skipped: rebuild_export is off"
	}
	if err := rb.step(ctx, "export", fleet.StepExportWorkspace, detail); err != nil {
		return err
	}
	if !rb.running() {
		return rb.exportStopped(ctx, mode != RebuildExportOff)
	}
	// The daemon's build of fleet in the pod (it has the export).
	t := rb.prev.target()
	t.Priority, t.Restage = PriorityInteractive, true
	sess, err := rb.p.EnsureAgent(ctx, t)
	if err != nil {
		return classify(fmt.Errorf("reaching the running instance's agent (a stopped instance is rebuilt from its disk): %w", err))
	}
	rb.sess = sess
	if mode == RebuildExportOff {
		return nil
	}
	return rb.exportRunning(ctx, sess)
}

// exportRunning exports the running pod's workspace over its bulk session.
func (rb *rebuilding) exportRunning(ctx context.Context, sess *AgentSession) error {
	if rb.prev.WorkspaceFolder == "" {
		return errors.New("fleet's record of the instance names no workspace folder to export")
	}
	bulk, err := sess.Bulk(ctx)
	if err != nil {
		return fmt.Errorf("the pod's bulk session: %w", err)
	}
	e, err := bulk.Exec(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: exportArgv(podseed.CurrentBinary, rb.prev.WorkspaceFolder)}})
	if err != nil {
		return fmt.Errorf("starting the workspace export: %w", startError(err))
	}
	defer e.Close()
	stop := context.AfterFunc(ctx, func() { _ = e.Close() })
	defer stop()
	_ = e.Stdin().Close()
	var stderr bytes.Buffer
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&limitedWriter{w: &stderr, n: 64 << 10}, e.Stderr())
		close(errDone)
	}()
	got, rerr := rb.receive(ctx, e.Stdout())
	if rerr != nil {
		_ = e.Close()
	}
	code, werr := e.Wait()
	<-errDone
	if werr == nil && code != 0 {
		werr = fmt.Errorf("exit code %d", code)
	}
	return rb.finishExport(ctx, got, rerr, werr, stderr.String())
}

// exportStopped runs a tool Job of the instance's generation on its disk:
// it reports the disk's room, and the export runs in its pod when wanted.
func (rb *rebuilding) exportStopped(ctx context.Context, export bool) error {
	s, err := rb.scope()
	if err != nil {
		return err
	}
	name := names.ExportJob(rb.id.sts, rb.prevGen)
	ri := rb.renderInput(rb.liveImage(), nil)
	ri.Gen, ri.CreatedAt = rb.prevGen, rb.prev.Created
	job, err := podspec.RenderJob(toolResult(rb.sts), ri, podspec.JobInput{Name: name, Resources: toolResources(), ActiveDeadline: exportHold + 10*time.Minute}, names.RoleTool)
	if err != nil {
		return classify(err)
	}
	if err := rb.p.freshJob(ctx, s, name); err != nil {
		return classify(err)
	}
	body, err := podspec.ApplyBody(job.Job)
	if err != nil {
		return err
	}
	if _, err := applyJob(ctx, rb.cs, rb.ns, body); err != nil {
		return classify(err)
	}
	defer rb.p.dropJob(s, name)

	want := job.Job.Spec.Template.Spec
	del := &kubeimage.Delivery{Strategy: kubeimage.Strategy(rb.prev.Strategy)}
	redelivered := false
	res, err := rb.p.RunJob(ctx, JobRun{
		Cluster: rb.id.cluster, Job: name,
		Spec: podseed.JobSpec{Op: podseed.OpTool, Gen: rb.prevGen, PVC: podspec.JobDiskPath, ResultHold: exportHold.String()},
		PVC:  rb.id.sts,
		// The instance's image the node lost while it was stopped (a
		// node-loaded one the kubelet's image GC collected) is delivered
		// again, as a start does.
		Waiting: func(ctx context.Context, pod *corev1.Pod) {
			if !redelivered && rb.b.redeliverFor(ctx, rb.prev, containerStatus(pod, podspec.JobContainer), "rebuild") {
				redelivered = true
			}
		},
		Check: func(pod *corev1.Pod) error {
			_, err := checkJobPod(pod, &want, del, rb.prev.expect())
			return err
		},
		While: func(ctx context.Context, exec JobExec) error {
			if !export {
				return nil
			}
			pr, pw := io.Pipe()
			var stderr bytes.Buffer
			done := make(chan error, 1)
			go func() {
				err := exec(ctx, exportArgv(jobboot.Paths{}.Binary(), path.Join(podspec.JobDiskPath, podseed.DirWorkspace)), pw, &limitedWriter{w: &stderr, n: 64 << 10})
				_ = pw.CloseWithError(err)
				done <- err
			}()
			got, rerr := rb.receive(ctx, pr)
			if rerr != nil {
				_ = pr.CloseWithError(rerr)
			}
			werr := <-done
			return rb.finishExport(ctx, got, rerr, werr, stderr.String())
		},
	})
	if err != nil {
		return classify(err)
	}
	if !res.OK {
		return fmt.Errorf("the tool Job on the instance disk failed: %s", res.Error)
	}
	var tr podseed.ToolResult
	if err := json.Unmarshal(res.Output, &tr); err != nil {
		return fmt.Errorf("the tool Job's result: %w", err)
	}
	rb.disk = &tr.Disk
	return nil
}

// toolResult is the translation a tool Job of the instance's current
// generation renders from, before the rebuild translated its new one: the
// placement the generation's pods have (node selector, runtime class); the
// rest of a tool Job (the connection's tolerations and service account, the
// identity, the image) comes from the render's input.
func toolResult(sts *appsv1.StatefulSet) *podspec.Result {
	res := &podspec.Result{}
	if sts == nil {
		// Its StatefulSet is gone: no placement of its own to keep.
		return res
	}
	tpl := sts.Spec.Template.Spec
	res.Pod.NodeSelector = maps.Clone(tpl.NodeSelector)
	if tpl.RuntimeClassName != nil {
		res.Pod.RuntimeClassName = *tpl.RuntimeClassName
	}
	return res
}

// received is what an export's receiving end got.
type received struct {
	dir   string
	size  int64
	sha   string
	stats podseed.WorkspaceStats
}

// exportDir is where an export of the checkout at wsDir arrives before it
// replaces it; oldDir where the checkout it replaces goes on the way out.
func exportDir(wsDir string) string { return wsDir + ".fleet-export" }
func oldDir(wsDir string) string    { return wsDir + ".fleet-old" }

// receive extracts an export from r beside the checkout (exportDir), the
// daemon user owning every entry, counting and hashing the stream.
func (rb *rebuilding) receive(ctx context.Context, r io.Reader) (received, error) {
	got := received{dir: exportDir(rb.req.WorkspaceDir)}
	if err := podseed.RemoveTree(got.dir); err != nil {
		return got, err
	}
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}
	owner := &podseed.IDs{UID: os.Getuid(), GID: os.Getgid()}
	st, err := podseed.ExtractWorkspace(cr, got.dir, owner, func(s podseed.WorkspaceStats) {
		backend.ReportStep(ctx, fleet.StepExportWorkspace, fmt.Sprintf("%d files", s.Files))
	})
	got.size, got.sha, got.stats = cr.n, hex.EncodeToString(h.Sum(nil)), st
	return got, err
}

// finishExport judges an export (the receiver's error, the export command's,
// its summary against what arrived) and, when it is whole, makes it the
// checkout.
func (rb *rebuilding) finishExport(ctx context.Context, got received, rerr, werr error, stderr string) error {
	sum, ok := podseed.ParseExportSummary(stderr)
	var err error
	switch {
	case werr != nil:
		err = fmt.Errorf("the workspace export: %w%s", werr, detail(stderr))
	case rerr != nil:
		err = fmt.Errorf("receiving the workspace export: %w", rerr)
	case !ok:
		err = fmt.Errorf("the workspace export ended without its summary%s", detail(stderr))
	case sum.Size != got.size || sum.SHA256 != got.sha:
		err = fmt.Errorf("the workspace export arrived damaged: %d bytes with sha256 %s, the instance sent %d with %s", got.size, got.sha, sum.Size, sum.SHA256)
	}
	if err != nil {
		_ = podseed.RemoveTree(got.dir)
		return err
	}
	if err := swapCheckout(rb.req.WorkspaceDir, got.dir); err != nil {
		_ = podseed.RemoveTree(got.dir)
		return fmt.Errorf("replacing the daemon host's checkout with the export: %w", err)
	}
	if len(sum.Unreadable) > 0 {
		rb.warn(ctx, fmt.Sprintf("the workspace export left out %d entries it could not read: %s", sum.Skipped, strings.Join(sum.Unreadable, "; ")))
	}
	if len(sum.Mounts) > 0 {
		fmt.Fprintf(backend.JobLogFrom(ctx), "the workspace export left the mounts inside the workspace empty: %s\n", strings.Join(sum.Mounts, ", "))
	}
	backend.ReportStep(ctx, fleet.StepExportWorkspace, fmt.Sprintf("%d files, %s", sum.Files, humanBytes(sum.Bytes)))
	flog.Info("kube rebuild: exported the workspace", "fleet", rb.id.fleet, "instance", rb.id.name, "files", sum.Files, "bytes", sum.Bytes, "skipped", sum.Skipped)
	return nil
}

// swapCheckout makes dir (an export beside the checkout) the checkout at
// wsDir, the old one deleted once it is out of the way.
func swapCheckout(wsDir, dir string) error {
	old := oldDir(wsDir)
	if err := podseed.RemoveTree(old); err != nil {
		return err
	}
	had := true
	if err := os.Rename(wsDir, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		had = false
	}
	if err := os.Rename(dir, wsDir); err != nil {
		if had {
			_ = os.Rename(old, wsDir)
		}
		return err
	}
	if had {
		if err := podseed.RemoveTree(old); err != nil {
			flog.Warn("kube rebuild: deleting the replaced checkout", "dir", old, "err", err)
		}
	}
	return nil
}

// countingReader counts what is read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// checkDisk makes sure the instance disk has room for the new generation's
// root beside the current one (which goes to trash, not away, until the
// rebuild succeeds): the image's size plus 10%. A disk short of it grows
// where the class can and auto_expand_disk is on; else the rebuild fails
// here, before anything changed. A disk whose room cannot be read is
// rebuilt on regardless: a seed that runs out of room is undone.
func (rb *rebuilding) checkDisk(ctx context.Context, img *built) error {
	size := rb.imageSize(ctx, img.capture.Image.ID)
	if size <= 0 {
		fmt.Fprintln(backend.JobLogFrom(ctx), "the new image's size is not known; the disk's room is not checked")
		return nil
	}
	need := size + size*rebuildHeadroom/100
	disk, err := rb.measureDisk(ctx)
	if err != nil {
		rb.warn(ctx, fmt.Sprintf("the instance disk's free space could not be read (%v); rebuilding regardless", err))
		return nil
	}
	fmt.Fprintf(backend.JobLogFrom(ctx), "instance disk: %s free of %s; generation %d's root needs %s (the image plus %d%%)\n", humanBytes(disk.Free), humanBytes(disk.Total), rb.gen, humanBytes(need), rebuildHeadroom)
	if disk.Free >= need {
		return nil
	}
	return rb.growDisk(ctx, disk, need)
}

// measureDisk is the instance disk's room: what a stopped instance's tool
// Job found, else what the running pod says (statfs of /opt/fleet, on the
// disk).
func (rb *rebuilding) measureDisk(ctx context.Context) (podseed.DiskSpace, error) {
	if rb.disk != nil {
		return *rb.disk, nil
	}
	sess := rb.sess
	if sess == nil || !sess.alive() {
		t := rb.prev.target()
		t.Priority = PriorityInteractive
		var err error
		if sess, err = rb.p.EnsureAgent(ctx, t); err != nil {
			return podseed.DiskSpace{}, err
		}
	}
	res, err := sess.Client().Run(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassRoot, Argv: []string{podseed.CurrentBinary, "pod-job", "statfs", "--path", podseed.InstallDir}}}, nil)
	if err != nil {
		return podseed.DiskSpace{}, err
	}
	if res.Code != 0 {
		return podseed.DiskSpace{}, fmt.Errorf("statfs exited %d%s", res.Code, detail(string(res.Stderr)))
	}
	var d podseed.DiskSpace
	if err := json.Unmarshal(res.Stdout, &d); err != nil {
		return podseed.DiskSpace{}, fmt.Errorf("statfs: %w", err)
	}
	return d, nil
}

// growDisk grows the instance disk by what the new root lacks, at least by
// half (auto_expand_disk's step), when the connection allows it and the
// storage class can, and waits for the cluster to take the new size (a
// filesystem resized only at its next mount counts: the seed mounts it).
func (rb *rebuilding) growDisk(ctx context.Context, disk podseed.DiskSpace, need int64) error {
	short := fmt.Sprintf("the instance disk has %s free and generation %d's root needs %s (the image plus %d%%)", humanBytes(disk.Free), rb.gen, humanBytes(need), rebuildHeadroom)
	if !rb.rec.AutoExpandsDisk() {
		return fmt.Errorf("%s, and auto_expand_disk is off: free room in the instance, or turn auto_expand_disk on where its storage class can grow disks", short)
	}
	pvcs := rb.cs.CoreV1().PersistentVolumeClaims(rb.ns)
	pvc, err := pvcs.Get(ctx, rb.id.sts, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("reading the instance disk: %w", err))
	}
	class := ""
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	if class == "" {
		return fmt.Errorf("%s, and the disk has no storage class to grow it", short)
	}
	sc, err := rb.cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return classify(fmt.Errorf("%s; reading storage class %s: %w", short, class, err))
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		return fmt.Errorf("%s, and storage class %s cannot grow disks: free room in the instance", short, class)
	}
	cur := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	size := grownSize(cur.Value(), need-disk.Free)
	q := resource.NewQuantity(size, resource.BinarySI)
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":%q}}}}`, q.String())
	if _, err := pvcs.Patch(ctx, pvc.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
		return classify(fmt.Errorf("%s; growing it: %w", short, err))
	}
	backend.ReportStep(ctx, fleet.StepApplyManifests, "growing the instance disk to "+q.String())
	deadline := rb.p.opts.Now().Add(DefaultDeadlines().PVCBind)
	for {
		got, err := pvcs.Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			return classify(fmt.Errorf("reading the instance disk: %w", err))
		}
		if c := got.Status.Capacity[corev1.ResourceStorage]; c.Value() >= size || resizePending(got) {
			break
		}
		if rb.p.opts.Now().After(deadline) {
			return &DeadlineError{Kind: CodeTimeout, Wait: "the instance disk to grow to " + q.String(), After: DefaultDeadlines().PVCBind, Env: EnvPVCBindTimeout}
		}
		if err := sleepCtx(ctx, DefaultWaitInterval); err != nil {
			return err
		}
	}
	rb.warn(ctx, fmt.Sprintf("the instance disk grew from %s to %s to hold generation %d's root", cur.String(), q.String(), rb.gen))
	return nil
}

// grownSize is a disk of cur bytes grown by at least add, and by half at
// least, in whole GiB.
func grownSize(cur, add int64) int64 {
	size := max(cur+cur/2, cur+add)
	const gi = 1 << 30
	return (size + gi - 1) / gi * gi
}

// resizePending reports whether a claim's resize waits only on the node's
// filesystem resize, which runs when a pod next mounts it.
func resizePending(pvc *corev1.PersistentVolumeClaim) bool {
	for _, c := range pvc.Status.Conditions {
		if c.Type == corev1.PersistentVolumeClaimFileSystemResizePending && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// humanBytes is n bytes for a person.
func humanBytes(n int64) string {
	const gi, mi = 1 << 30, 1 << 20
	switch {
	case n >= gi:
		return fmt.Sprintf("%.1f GiB", float64(n)/gi)
	case n >= mi:
		return fmt.Sprintf("%.1f MiB", float64(n)/mi)
	}
	return fmt.Sprintf("%d bytes", n)
}
