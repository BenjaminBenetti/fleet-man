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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// TestLiveRebuild rebuilds instances on a real cluster through the backend
// handle: from running and from stopped (the workspace exported over the
// bulk session or by a tool Job, the devcontainer.json edited in the pod
// built, the root reset, the workspace kept, the generation bumped, the
// previous generation's Secrets and trash deleted); a rebuild whose new
// postCreateCommand fails, undone to generation 1 with its root; and the
// rebuild of a create that failed, which is its retry. It needs what
// TestLiveBackendLifecycle needs (FLEET_KUBE_TEST_CONTEXT, docker, the
// devcontainer CLI); namespaces fleet-it-p6-01-rebuild-* are deleted when it
// ends.
func TestLiveRebuild(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to rebuild instances on a cluster")
	}
	for _, tool := range []string{"docker", "devcontainer"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	lb := newLiveBackend(t, kctx)
	lv := lb.connectIn(t, "fleet-it-p6-01-rebuild-", nil, true)

	t.Run("running", func(t *testing.T) { lb.rebuild(t, lv, "rbrun", image, false) })
	t.Run("stopped", func(t *testing.T) { lb.rebuild(t, lv, "rbstop", image, true) })
	t.Run("rollback", func(t *testing.T) { lb.rebuildRollback(t, lv, image) })
	t.Run("retry", func(t *testing.T) { lb.retryFailedCreate(t, lv, image) })
	t.Run("stopped, image gone from the node", func(t *testing.T) { lb.rebuildStoppedImageGone(t, lv, image) })
	t.Run("statefulset deleted", func(t *testing.T) { lb.rebuildLostStatefulSet(t, lv, image) })
}

// rebuildLostStatefulSet: an instance whose StatefulSet someone deleted
// (kubectl delete sts; its pod goes, its disk stays) is rebuilt over its
// disk: a StatefulSet of the next generation, the workspace kept.
func (lb *liveBackend) rebuildLostStatefulSet(t *testing.T, lv *live, image string) {
	r := lb.rebuildRig(t, lv, "rblost", map[string]any{"image": image})
	r.create()
	folder := "/workspaces/" + filepath.Base(r.inst.WorkspaceDir)
	r.sh("echo kept > " + folder + "/UNCOMMITTED.txt")
	old := r.inst.ContainerID
	background := metav1.DeletePropagationBackground
	if err := lv.cs.AppsV1().StatefulSets(lv.ns).Delete(r.ctx, r.sts, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, err := lv.cs.CoreV1().Pods(lv.ns).Get(r.ctx, names.Pod(r.sts), metav1.GetOptions{})
		if gone(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the deleted StatefulSet's pod is still there: %v", err)
		}
		time.Sleep(time.Second)
	}
	res, err := r.rebuild()
	if err != nil {
		t.Fatalf("the rebuild of an instance whose StatefulSet was deleted: %v", err)
	}
	if want := ContainerID(lv.rec.ID, lv.ns, r.sts, 2); res.ContainerID != want || res.ContainerID == old {
		t.Fatalf("rebuilt ContainerID %s, want %s", res.ContainerID, want)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "deleted outside fleet") }) {
		t.Errorf("no warning that the StatefulSet was gone: %q", res.Warnings)
	}
	if got := r.b().Status(res.ContainerID); got != backend.LiveStatusRunning {
		t.Errorf("the rebuilt generation's status %v, want running", got)
	}
	if got := r.sh("cat " + folder + "/UNCOMMITTED.txt"); got != "kept" {
		t.Errorf("the workspace file after the rebuild: %q", got)
	}
	if _, err := os.Stat(JournalPath(r.inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the rebuild's journal is still there: %v", err)
	}
}

// rebuildStoppedImageGone: a stopped instance whose node-loaded image the
// node lost (the kubelet's image GC) is rebuilt all the same: the export's
// tool Job runs that image, which is delivered again, as a start does. A
// minikube connection only (the node's image is removed with minikube).
func (lb *liveBackend) rebuildStoppedImageGone(t *testing.T, lv *live, image string) {
	local := lv.facts(t).Local
	if local.Kind != localcluster.Minikube {
		t.Skipf("needs a minikube connection to remove a node's image (this one is %q)", local.String())
	}
	r := lb.rebuildRig(t, lv, "rbgone", map[string]any{"build": map[string]any{"dockerfile": "Dockerfile"}})
	dockerfile := func(gen string) string { return "FROM " + image + "\nRUN echo " + gen + " > /etc/fleet-live-gen\n" }
	if err := os.WriteFile(filepath.Join(r.inst.WorkspaceDir, ".devcontainer", "Dockerfile"), []byte(dockerfile("gen1")), 0o644); err != nil {
		t.Fatal(err)
	}
	r.create()
	rec, err := LoadInstanceRecord(lv.rec.ID, r.inst.KubeKey)
	if err != nil {
		t.Fatal(err)
	}
	if kubeimage.Strategy(rec.Strategy) != kubeimage.StrategyNodeLoad {
		t.Skipf("the image was delivered by %s, not loaded onto the node", rec.Strategy)
	}
	folder := "/workspaces/" + filepath.Base(r.inst.WorkspaceDir)
	r.sh("cat > " + folder + "/.devcontainer/Dockerfile <<'FLEET_EOF'\n" + dockerfile("gen2") + "FLEET_EOF")
	if err := r.b().Stop(r.inst.ContainerID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if out, err := exec.Command("minikube", "-p", local.Name, "image", "rm", rec.Image).CombinedOutput(); err != nil {
		t.Fatalf("removing %s from the node: %v\n%s", rec.Image, err, out)
	}
	if _, err := r.rebuild(); err != nil {
		t.Fatalf("the rebuild of a stopped instance whose image the node lost: %v", err)
	}
	if got := r.sh("cat /etc/fleet-live-gen"); got != "gen2" {
		t.Errorf("the rebuilt instance's image says %q, want gen2 (the Dockerfile edited in the pod)", got)
	}
}

// rebuildRig is one instance a rebuild test drives.
type rebuildRig struct {
	t    *testing.T
	lb   *liveBackend
	lv   *live
	inst *fleet.Instance
	sts  string
	ctx  context.Context
}

func (lb *liveBackend) rebuildRig(t *testing.T, lv *live, name string, cfg map[string]any) *rebuildRig {
	inst, sts := lb.instance(t, lv, name, func(ws string) { writeLiveWorkspace(t, ws, cfg) })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	r := &rebuildRig{t: t, lb: lb, lv: lv, inst: inst, sts: sts, ctx: ctx}
	t.Cleanup(func() {
		if err := r.b().Down(r.inst.ContainerID); err != nil {
			t.Errorf("down: %v", err)
		}
		lb.waitNothingLeft(t, lv, inst.KubeKey)
	})
	return r
}

func (r *rebuildRig) b() *Backend { return newHandle(r.inst, r.lb.pool) }

func (r *rebuildRig) create() {
	r.t.Helper()
	res, err := r.b().UpContext(backend.WithJobLog(r.ctx, &lineLog{t: r.t}), backend.UpRequest{WorkspaceDir: r.inst.WorkspaceDir})
	if err != nil {
		r.t.Fatalf("create: %v", err)
	}
	r.inst.ContainerID, r.inst.Status = res.ContainerID, fleet.StatusRunning
}

// rebuild runs the rebuild a rebuild job runs; on success the record takes
// its ContainerID.
func (r *rebuildRig) rebuild() (*backend.UpResult, error) {
	r.t.Helper()
	start := time.Now()
	res, err := r.b().RebuildContext(backend.WithJobLog(r.ctx, &lineLog{t: r.t}), r.inst.ContainerID, backend.UpRequest{WorkspaceDir: r.inst.WorkspaceDir})
	r.t.Logf("rebuild took %v", time.Since(start).Round(time.Second))
	if err == nil {
		r.inst.ContainerID, r.inst.Status = res.ContainerID, fleet.StatusRunning
	}
	return res, err
}

func (r *rebuildRig) sh(script string) string {
	r.t.Helper()
	out, err := r.b().RunScript(r.inst.ContainerID, script)
	if err != nil {
		r.t.Fatalf("%q: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(out)
}

// editConfig rewrites the devcontainer.json in the pod's workspace, as a
// user edits it there.
func (r *rebuildRig) editConfig(cfg map[string]any) {
	r.t.Helper()
	full := map[string]any{"remoteUser": "vscode", "postCreateCommand": "echo created >> ~/.lifecycle", "postStartCommand": "echo started >> ~/.lifecycle"}
	for k, v := range cfg {
		full[k] = v
	}
	data, _ := json.Marshal(full)
	r.sh("cat > /workspaces/" + filepath.Base(r.inst.WorkspaceDir) + "/.devcontainer/devcontainer.json <<'FLEET_EOF'\n" + string(data) + "\nFLEET_EOF")
}

// diskEntries lists the top-level entries of the instance disk, from a pod
// of the instance's image beside its pod (on its node).
func (r *rebuildRig) diskEntries() []string {
	r.t.Helper()
	ctx := r.ctx
	set, err := r.lv.cs.AppsV1().StatefulSets(r.lv.ns).Get(ctx, r.sts, metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	var image string
	var policy corev1.PullPolicy
	for _, c := range set.Spec.Template.Spec.Containers {
		if c.Name == DevContainer {
			image, policy = c.Image, c.ImagePullPolicy
		}
	}
	node := ""
	if pod, err := r.lv.cs.CoreV1().Pods(r.lv.ns).Get(ctx, names.Pod(r.sts), metav1.GetOptions{}); err == nil {
		node = pod.Spec.NodeName
	}
	name := fmt.Sprintf("%s-ls-%d", r.sts, time.Now().UnixNano()%100000)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.lv.ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, NodeName: node, ImagePullSecrets: set.Spec.Template.Spec.ImagePullSecrets,
			Containers: []corev1.Container{{Name: "ls", Image: image, ImagePullPolicy: policy, Command: []string{"ls", "-1", "/pvc"},
				VolumeMounts: []corev1.VolumeMount{{Name: "disk", MountPath: "/pvc", ReadOnly: true}}}},
			Volumes: []corev1.Volume{{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.sts, ReadOnly: true}}}},
		},
	}
	if _, err := r.lv.cs.CoreV1().Pods(r.lv.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = r.lv.cs.CoreV1().Pods(r.lv.ns).Delete(context.Background(), name, metav1.DeleteOptions{}) }()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		got, err := r.lv.cs.CoreV1().Pods(r.lv.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			r.t.Fatal(err)
		}
		if got.Status.Phase == corev1.PodSucceeded {
			break
		}
		if got.Status.Phase == corev1.PodFailed || time.Now().After(deadline) {
			r.t.Fatalf("the disk listing pod: %s %+v", got.Status.Phase, got.Status.ContainerStatuses)
		}
		time.Sleep(time.Second)
	}
	out, err := r.lv.cs.CoreV1().Pods(r.lv.ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// waitDisk waits until the instance disk's top level has every entry of want
// and none of unwanted (a successful rebuild deletes its trash in the
// background).
func (r *rebuildRig) waitDisk(want, unwanted []string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		entries := r.diskEntries()
		ok := true
		for _, w := range want {
			ok = ok && slices.Contains(entries, w)
		}
		for _, u := range unwanted {
			ok = ok && !slices.Contains(entries, u)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the instance disk holds %v, want %v and none of %v", entries, want, unwanted)
		}
		time.Sleep(3 * time.Second)
	}
}

// waitGone waits for the objects of the previous generation a successful
// rebuild deletes in the background (its Secrets, the commit Job).
func (r *rebuildRig) waitGone(objects ...string) {
	r.t.Helper()
	scope, err := r.lb.pool.Scope(r.lv.rec.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		left, err := scope.ListByKey(context.Background(), r.inst.KubeKey)
		if err != nil {
			r.t.Fatal(err)
		}
		var still []string
		for _, o := range left {
			for _, want := range objects {
				if objectRef(o.Kind, o.Name) == want {
					still = append(still, want)
				}
			}
		}
		if len(still) == 0 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("still there after a successful rebuild: %v", still)
		}
		time.Sleep(2 * time.Second)
	}
}

// rebuild: an instance whose devcontainer.json was edited in its pod is
// rebuilt from running (or stopped): the edit is built, the root is the new
// image's, the workspace and the generation's ID move on.
func (lb *liveBackend) rebuild(t *testing.T, lv *live, name, image string, fromStopped bool) {
	r := lb.rebuildRig(t, lv, name, map[string]any{"image": image})
	r.create()
	folder := "/workspaces/" + filepath.Base(r.inst.WorkspaceDir)
	r.sh("echo kept > " + folder + "/UNCOMMITTED.txt && sudo sh -c 'echo gen1 > /usr/local/bin/fleet-gen-marker'")
	r.editConfig(map[string]any{"image": image, "containerEnv": map[string]string{"FLEET_REBUILT": "yes"}})
	old := r.inst.ContainerID
	if fromStopped {
		if err := r.b().Stop(old); err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	res, err := r.rebuild()
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if want := ContainerID(lv.rec.ID, lv.ns, r.sts, 2); res.ContainerID != want {
		t.Fatalf("rebuilt ContainerID %s, want %s", res.ContainerID, want)
	}
	if got := r.b().Status(old); got != backend.LiveStatusMissing {
		t.Errorf("the replaced generation's status %v, want missing", got)
	}
	if got := r.b().Status(res.ContainerID); got != backend.LiveStatusRunning {
		t.Errorf("the rebuilt generation's status %v, want running", got)
	}
	if _, err := os.Stat(JournalPath(r.inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the rebuild's journal is still there: %v", err)
	}
	if rec, err := LoadInstanceRecord(lv.rec.ID, r.inst.KubeKey); err != nil || rec.Gen != 2 {
		t.Errorf("fleet's record after the rebuild: gen %d, %v", rec.Gen, err)
	}
	// The edit made in the pod reached the daemon host's checkout and was
	// built from there.
	if data, err := os.ReadFile(filepath.Join(r.inst.WorkspaceDir, ".devcontainer", "devcontainer.json")); err != nil || !strings.Contains(string(data), "FLEET_REBUILT") {
		t.Errorf("the host checkout's devcontainer.json %s (%v) lacks the edit made in the pod", data, err)
	}
	if got := r.sh("printenv FLEET_REBUILT || true"); got != "yes" {
		t.Errorf("the rebuilt container's FLEET_REBUILT %q, want the edited containerEnv", got)
	}
	if got := r.sh("cat " + folder + "/UNCOMMITTED.txt"); got != "kept" {
		t.Errorf("the workspace file after the rebuild: %q", got)
	}
	if got := r.sh("cat /usr/local/bin/fleet-gen-marker 2>/dev/null || echo gone"); got != "gone" {
		t.Errorf("generation 1's root survived the rebuild: %q", got)
	}
	if got := r.sh("command -v fleet"); got != "/usr/bin/fleet" {
		t.Errorf("command -v fleet after the rebuild: %q", got)
	}
	if got := r.sh("cat ~/.lifecycle"); got != "created\nstarted" {
		t.Errorf("hooks of the rebuilt container: %q (a new container runs its creation hooks)", got)
	}
	// The commit deletes generation 1's root (in trash since the reset)
	// once the rebuild succeeded, and its Secrets go.
	r.waitDisk([]string{podseed.DirRoot, podseed.DirWorkspace}, []string{"root.g1.trash", "root-extra.g1.trash"})
	r.waitGone(objectRef(KindSecret, names.SpecSecret(r.sts, 1)), objectRef(KindSecret, names.EnvSecret(r.sts, 1)),
		objectRef(KindJob, names.CommitJob(r.sts, 2)), objectRef(KindJob, names.ExportJob(r.sts, 1)))
}

// rebuildRollback: a rebuild whose edited postCreateCommand fails is undone:
// generation 1 runs again with its root, its ID and record; the next rebuild,
// with the edit fixed, succeeds.
func (lb *liveBackend) rebuildRollback(t *testing.T, lv *live, image string) {
	r := lb.rebuildRig(t, lv, "rbroll", map[string]any{"image": image})
	r.create()
	folder := "/workspaces/" + filepath.Base(r.inst.WorkspaceDir)
	r.sh("echo kept > " + folder + "/UNCOMMITTED.txt && sudo sh -c 'echo gen1 > /usr/local/bin/fleet-gen-marker'")
	r.editConfig(map[string]any{"image": image, "postCreateCommand": "echo the edit broke the hook >&2; exit 7"})
	old := r.inst.ContainerID
	_, err := r.rebuild()
	if err == nil {
		t.Fatal("a rebuild whose postCreateCommand fails succeeded")
	}
	t.Logf("failed as expected: %v", err)
	if code, _ := kubeerr.Of(err); !code.Is(kubeerr.RebuildFailed) || !strings.Contains(err.Error(), "generation 1 was restored") {
		t.Fatalf("the failed rebuild's error %v, want kubernetes:rebuild_failed naming the restored generation", err)
	}
	if errors.Is(err, backend.ErrCheckpointed) {
		t.Fatal("an in-process failure left a checkpoint")
	}
	if _, err := os.Stat(JournalPath(r.inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the undone rebuild's journal is still there: %v", err)
	}
	if rec, err := LoadInstanceRecord(lv.rec.ID, r.inst.KubeKey); err != nil || rec.Gen != 1 {
		t.Fatalf("fleet's record after the undo: gen %d, %v", rec.Gen, err)
	}
	waitStatus(t, r.b(), old, backend.LiveStatusRunning)
	if got := r.sh("cat /usr/local/bin/fleet-gen-marker " + folder + "/UNCOMMITTED.txt"); got != "gen1\nkept" {
		t.Errorf("after the undo generation 1's root and the workspace read %q", got)
	}
	set, err := lv.cs.AppsV1().StatefulSets(lv.ns).Get(r.ctx, r.sts, metav1.GetOptions{})
	if err != nil || set.Annotations[names.AnnotationRebuildGen] != "1" || set.Annotations[names.AnnotationOp] != "" {
		t.Errorf("the StatefulSet after the undo: %v %v", set.Annotations, err)
	}
	r.waitGone(objectRef(KindSecret, names.SpecSecret(r.sts, 2)), objectRef(KindJob, names.SeedJob(r.sts, 2)), objectRef(KindJob, names.RollbackJob(r.sts, 1)))
	// The rollback put generation 1's root back from trash.
	r.waitDisk([]string{podseed.DirRoot}, []string{"root.g1.trash"})

	// The fix, then the retry.
	r.editConfig(map[string]any{"image": image})
	res, err := r.rebuild()
	if err != nil {
		t.Fatalf("the rebuild after the fix: %v", err)
	}
	if want := ContainerID(lv.rec.ID, lv.ns, r.sts, 2); res.ContainerID != want {
		t.Errorf("ContainerID %s, want %s", res.ContainerID, want)
	}
	if got := r.sh("cat " + folder + "/UNCOMMITTED.txt; cat /usr/local/bin/fleet-gen-marker 2>/dev/null || echo gone"); got != "kept\ngone" {
		t.Errorf("after the second rebuild: %q", got)
	}
}

// retryFailedCreate: the rebuild of a create that failed (no ContainerID)
// deletes what it left and creates it again from the host checkout.
func (lb *liveBackend) retryFailedCreate(t *testing.T, lv *live, image string) {
	r := lb.rebuildRig(t, lv, "rbretry", map[string]any{"image": image, "postCreateCommand": "exit 3"})
	if _, err := r.b().UpContext(backend.WithJobLog(r.ctx, &lineLog{t: r.t}), backend.UpRequest{WorkspaceDir: r.inst.WorkspaceDir}); err == nil {
		t.Fatal("a create whose postCreateCommand fails succeeded")
	}
	r.inst.Status = fleet.StatusFailed
	writeLiveWorkspace(t, r.inst.WorkspaceDir, map[string]any{"image": image})
	res, err := r.rebuild()
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if want := ContainerID(lv.rec.ID, lv.ns, r.sts, 1); res.ContainerID != want {
		t.Errorf("the retried create's ContainerID %s, want %s", res.ContainerID, want)
	}
	if got := r.sh("cat ~/.lifecycle"); got != "created\nstarted" {
		t.Errorf("hooks of the retried create: %q", got)
	}
}

// waitStatus waits for b's status of id to be want.
func waitStatus(t *testing.T, b *Backend, id string, want backend.LiveStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		got := b.Status(id)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status of %s: %v, want %v", id, got, want)
		}
		time.Sleep(time.Second)
	}
}
