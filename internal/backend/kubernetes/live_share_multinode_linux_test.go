//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveShareMultiNode runs a fleet in multi-node mode on a cluster of two
// nodes or more whose FLEET_KUBE_TEST_RWX_CLASS passed the multi-node test
// (csi-driver-nfs's): the fleet's share is a ReadWriteMany volume of it, its
// pods are placed by nothing but the volume (alpha's node is cordoned while
// beta is made, so they run on two nodes), and they see each other's writes
// at once; Codex's SQLite state is each pod's own (CODEX_SQLITE_HOME below
// the user's home); a write in beta reaches the daemon host through the
// spoke in alpha's pod well before the spoke's minute rescan, by the hint
// beta's agent sends. Gated like TestLiveShare, and on the class
// (FLEET_KUBE_TEST_DISK_CLASS picks the disks', "" the default); it leaves
// the nodes uncordoned.
func TestLiveShareMultiNode(t *testing.T) {
	kctx, class := os.Getenv("FLEET_KUBE_TEST_CONTEXT"), os.Getenv("FLEET_KUBE_TEST_RWX_CLASS")
	if kctx == "" || class == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> of two nodes and FLEET_KUBE_TEST_RWX_CLASS=<class> to run a multi-node fleet share")
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	lb := newLiveBackend(t, kctx)
	lb.prefix = "fleet-it-p4-04a-mn-"
	// The instance disks need a class whose volumes follow their pods (a
	// WaitForFirstConsumer one, as kind's local-path): minikube's hostpath
	// class, the default there, is refused in multi-node mode.
	lv := lb.connect(t, nil, true, func(rec *fleet.KubeCluster) { rec.StorageClass = os.Getenv("FLEET_KUBE_TEST_DISK_CLASS") })
	lb.classifyStorage(t, lv)
	lb.mu.Lock()
	caps := lb.facts[lv.rec.ID].Capabilities
	caps.ShareMode, caps.SharedStorageClass = string(topology.MultiNode), class
	for _, sc := range caps.StorageClasses {
		if sc.GetName() == class {
			sc.NodeLocal = false
		}
	}
	lb.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	resolved, err := resolver.Resolve("live", fleet.FleetSettings{ClaudeCodeMount: true, CodexMount: true, HomeDir: "/home/vscode"})
	if err != nil {
		t.Fatal(err)
	}
	type made struct {
		inst *fleet.Instance
		sts  string
		res  *backend.UpResult
		b    *Backend
	}
	up := func(name string) *made {
		t.Helper()
		inst, sts := lb.instance(t, lv, name, func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) })
		b := newHandle(inst, lb.pool)
		res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir, Mounts: resolved.Mounts, Layout: resolved.Layout})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
		return &made{inst: inst, sts: sts, res: res, b: newHandle(inst, lb.pool)}
	}
	var all []*made
	defer func() {
		for _, m := range all {
			_ = m.b.Down(m.res.ContainerID)
		}
	}()
	sh := func(m *made, script string) string {
		t.Helper()
		out, err := m.b.RunScript(m.res.ContainerID, script)
		if err != nil {
			t.Fatalf("%s: %q: %v\n%s", m.inst.Name, script, err, out)
		}
		return strings.TrimSpace(out)
	}
	nodeOf := func(m *made) string {
		p, err := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, names.Pod(m.sts), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return p.Spec.NodeName
	}
	cordon := func(node string, on bool) {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"unschedulable": on}})
		if _, err := lv.cs.CoreV1().Nodes().Patch(context.Background(), node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatalf("cordon %s %v: %v", node, on, err)
		}
	}

	alpha := up("alpha")
	all = append(all, alpha)
	home := nodeOf(alpha)
	cordon(home, true)
	defer cordon(home, false)
	beta := up("beta")
	all = append(all, beta)
	cordon(home, false)
	if other := nodeOf(beta); other == home || other == "" {
		t.Fatalf("beta runs on %q beside alpha on %q", other, home)
	}

	h8 := names.FleetHash(lb.daemonID, lv.rec.ID, "live")
	claim := ShareClaim("live", h8)
	pvc, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the fleet's share: %v", err)
	}
	if pvc.Annotations[names.AnnotationShareMode] != string(topology.MultiNode) || ptrOr(pvc.Spec.StorageClassName) != class ||
		!reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}) {
		t.Fatalf("the share: class %s, modes %v, annotations %v", ptrOr(pvc.Spec.StorageClassName), pvc.Spec.AccessModes, pvc.Annotations)
	}
	for _, m := range all {
		p, _ := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, names.Pod(m.sts), metav1.GetOptions{})
		if a := p.Spec.Affinity; a != nil && (a.PodAffinity != nil || a.NodeAffinity != nil) {
			t.Errorf("%s is placed by %+v, want nothing (an NFS volume mounts anywhere)", m.inst.Name, a)
		}
		rec, err := LoadInstanceRecord(lv.rec.ID, m.inst.KubeKey)
		if err != nil || rec.Share == nil || rec.Share.Mode != string(topology.MultiNode) {
			t.Fatalf("%s's record share %+v (%v)", m.inst.Name, rec.Share, err)
		}
	}

	// One share across the nodes, live.
	sh(alpha, "echo from-alpha > ~/.claude/shared.txt")
	if got := sh(beta, "cat ~/.claude/shared.txt"); got != "from-alpha" {
		t.Errorf("beta on another node reads %q", got)
	}
	sh(beta, "echo from-beta >> ~/.claude/shared.txt")
	if got := sh(alpha, "cat ~/.claude/shared.txt"); got != "from-alpha\nfrom-beta" {
		t.Errorf("alpha reads %q after beta's append", got)
	}
	// Codex's SQLite is each pod's own.
	want := "/home/vscode/" + podspec.CodexSQLiteHome
	for _, m := range all {
		if got := sh(m, "echo $"+podspec.CodexSQLiteHomeEnv); got != want {
			t.Errorf("%s: %s = %q, want %q", m.inst.Name, podspec.CodexSQLiteHomeEnv, got, want)
		}
		stream, err := m.b.OpenExec(ctx, backend.ExecRequest{Class: backend.ExecClassExec, ContainerID: m.res.ContainerID, Argv: []string{"sh", "-c", "echo $" + podspec.CodexSQLiteHomeEnv}})
		if err != nil {
			t.Fatalf("OpenExec: %v", err)
		}
		_ = stream.Stdin().Close()
		out, _ := io.ReadAll(stream.Stdout())
		if code, err := stream.Wait(); err != nil || code != 0 || strings.TrimSpace(string(out)) != want {
			t.Errorf("%s: an exec's %s = %q (%d %v)", m.inst.Name, podspec.CodexSQLiteHomeEnv, out, code, err)
		}
	}

	// The spoke in alpha's pod hears of beta's writes by beta's hints.
	target, share, err := InstanceShare(lv.rec.ID, alpha.inst.KubeKey)
	if err != nil {
		t.Fatal(err)
	}
	hubDir := filepath.Join(state.FleetDir(), "sync-live")
	host := filepath.Join(state.WorkspacesDir(), "live")
	var treeRoots []sharesync.TreeRoot
	var rootNames []string
	for _, r := range share.Roots {
		treeRoots = append(treeRoots, sharesync.TreeRoot{Name: r.Name, Dir: filepath.Join(host, r.Name), Reserved: r.Reserved, Skip: r.Skip})
		rootNames = append(rootNames, r.Name)
	}
	for _, e := range resolved.Layout {
		if p := filepath.Join(host, e.Path); !e.Dir && e.Seed != "" {
			if fi, err := os.Stat(p); err == nil && fi.Size() == 0 {
				if err := os.WriteFile(p, []byte(e.Seed), 0o666); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	hub, err := sharesync.OpenHub(filepath.Join(hubDir, "hub"), syncpolicy.Defaults(), rootNames, sharesync.HubOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	tree, err := sharesync.OpenTree(treeRoots, sharesync.TreeOptions{StateDir: filepath.Join(hubDir, "host")})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	side := sharesync.NewSide(sharesync.SideInfo{ID: "host", Location: "host"}, tree, hub, sharesync.SideOptions{NoWatch: true})
	if err := side.Attach(ctx, hub); err != nil {
		t.Fatal(err)
	}
	link, err := lb.pool.OpenSpoke(ctx, target, alpha.inst.KubeKey, share)
	if err != nil {
		t.Fatalf("OpenSpoke: %v", err)
	}
	served := make(chan error, 1)
	attached := make(chan struct{})
	go func() {
		served <- lb.pool.ServeSpoke(ctx, link, hub, ShareSide(lv.rec.ID, alpha.inst.KubeKey, share), func(uint64) { close(attached) })
	}()
	waitAttached(t, attached)
	flushLive(t, ctx, hub)
	// The spoke's first scan is done: what follows is beta's write, which
	// alpha's watch cannot see.
	written := time.Now()
	sh(beta, "mkdir -p ~/.claude/projects/p && echo hinted > ~/.claude/projects/p/s.jsonl")
	deadline := written.Add(40 * time.Second)
	for {
		if data, err := os.ReadFile(filepath.Join(host, ".claude", "projects", "p", "s.jsonl")); err == nil && string(data) == "hinted\n" {
			t.Logf("beta's write reached the host in %s", time.Since(written).Round(100*time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("beta's write did not reach the host within %s (the spoke's rescan is a minute): no hint came", deadline.Sub(written))
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = link.Close()
	select {
	case <-served:
	case <-time.After(30 * time.Second):
		t.Error("the spoke's connection outlived its close")
	}

	for _, m := range all {
		if err := m.b.Down(m.res.ContainerID); err != nil {
			t.Errorf("down %s: %v", m.inst.Name, err)
		}
		lb.waitNothingLeft(t, lv, m.inst.KubeKey)
	}
	all = nil
}
