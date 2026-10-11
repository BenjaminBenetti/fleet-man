//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveShare runs a fleet with Claude Code's mounts and a custom mount on
// a real cluster in single-node mode: two instances created at once make one
// share, bound beside the first seed's disk and recorded; both pods run on
// the node holding it, mount the share's folders laid out for the share uid
// (the JSON settings file seeded '{}'), and see one inode; the tree's root
// is beyond the pods' users; a spoke in one pod syncs the share with a hub
// and the daemon host's tree both ways; a stop and start keep it; destroying
// the instances keeps the share for the fleet. It needs what
// TestLiveBackendLifecycle does (FLEET_KUBE_TEST_CONTEXT, docker, the
// devcontainer CLI); namespaces fleet-it-p4-03-share-* go when it ends.
func TestLiveShare(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run a fleet share on a cluster")
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	lb := newLiveBackend(t, kctx)
	lb.prefix = "fleet-it-p4-03-share-"
	lv := lb.connect(t, nil, true)
	lb.classifyStorage(t, lv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	resolved, err := resolver.Resolve("live", fleet.FleetSettings{ClaudeCodeMount: true, HomeDir: "/home/vscode", CustomMounts: []string{"/opt/data"}})
	if err != nil {
		t.Fatal(err)
	}
	type made struct {
		inst *fleet.Instance
		sts  string
		res  *backend.UpResult
		b    *Backend
	}
	var mu sync.Mutex
	got := map[string]*made{}
	var wg sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		inst, sts := lb.instance(t, lv, name, func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) })
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := newHandle(inst, lb.pool)
			res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir, Mounts: resolved.Mounts, Layout: resolved.Layout})
			if err != nil {
				t.Errorf("create %s: %v", name, err)
				return
			}
			inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
			mu.Lock()
			got[name] = &made{inst: inst, sts: sts, res: res, b: newHandle(inst, lb.pool)}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	alpha, beta := got["alpha"], got["beta"]
	defer func() {
		for _, m := range got {
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

	h8 := names.FleetHash(lb.daemonID, lv.rec.ID, "live")
	claim := ShareClaim("live", h8)
	pvc, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the fleet's share: %v", err)
	}
	if pvc.Annotations[names.AnnotationShareMode] != string(topology.SingleNode) || pvc.Annotations[names.AnnotationShareTopology] == "" {
		t.Errorf("the share's annotations %v, want its mode and recorded topology", pvc.Annotations)
	}
	t.Logf("share %s in class %v, topology %s", claim, ptrOr(pvc.Spec.StorageClassName), pvc.Annotations[names.AnnotationShareTopology])
	pa, _ := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, names.Pod(alpha.sts), metav1.GetOptions{})
	pb, _ := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, names.Pod(beta.sts), metav1.GetOptions{})
	if pa.Spec.NodeName == "" || pa.Spec.NodeName != pb.Spec.NodeName {
		t.Errorf("the fleet's pods run on %q and %q, want the share's node", pa.Spec.NodeName, pb.Spec.NodeName)
	}
	if a := pa.Spec.Affinity; a == nil || a.PodAffinity == nil {
		t.Errorf("an instance pod has no affinity to the share's holders: %+v", a)
	}
	for _, m := range []*made{alpha, beta} {
		rec, err := LoadInstanceRecord(lv.rec.ID, m.inst.KubeKey)
		if err != nil || rec.Share == nil || rec.Share.Claim != claim || rec.Share.Mode != "single-node" {
			t.Fatalf("%s's record share %+v (%v)", m.inst.Name, rec.Share, err)
		}
	}

	uid, _ := shareIDs()
	if got := sh(alpha, "stat -c %u ~/.claude /opt/data /fleet-mounts/files/.claude.json | sort -u"); got != fmt.Sprint(uid) {
		t.Errorf("the share's folders are owned by %q, want the share uid %d", got, uid)
	}
	// The create job links ~/.claude.json to it after the backend's create
	// (finishProvision); the share has it seeded before the pod starts.
	if got := sh(alpha, "cat /fleet-mounts/files/.claude.json"); got != "{}" {
		t.Errorf("the shared .claude.json holds %q, want the '{}' seed", got)
	}
	if got := sh(alpha, "ls "+podshare.SyncDir+" 2>&1 >/dev/null; echo $?"); !strings.HasSuffix(got, "2") && !strings.Contains(got, "ermission denied") {
		t.Errorf("the pod's user reaches the share's root: %q", got)
	}
	sh(alpha, "echo from-alpha > ~/.claude/shared.txt && echo data > /opt/data/d.txt")
	if a, b := sh(alpha, "stat -c %i ~/.claude/shared.txt"), sh(beta, "stat -c %i ~/.claude/shared.txt"); a != b || sh(beta, "cat ~/.claude/shared.txt /opt/data/d.txt") != "from-alpha\ndata" {
		t.Errorf("the siblings do not share one inode: %s vs %s", a, b)
	}

	// The share's spoke, run in alpha's pod, with a hub and the daemon
	// host's tree here.
	target, share, err := InstanceShare(lv.rec.ID, alpha.inst.KubeKey)
	if err != nil {
		t.Fatal(err)
	}
	hubDir := filepath.Join(state.FleetDir(), "sync-live")
	var treeRoots []sharesync.TreeRoot
	var rootNames []string
	host := filepath.Join(state.WorkspacesDir(), "live")
	for _, r := range share.Roots {
		treeRoots = append(treeRoots, sharesync.TreeRoot{Name: r.Name, Dir: filepath.Join(host, r.Name), Reserved: r.Reserved, Skip: r.Skip})
		rootNames = append(rootNames, r.Name)
	}
	// The resolver makes the host's single files empty, and a docker
	// instance's create seeds them; with only cluster instances nothing
	// does, and an empty JSON settings file is a write under way to the
	// hub, which never lands the share's copy over it. The daemon's sync
	// manager seeds them before it attaches the host (P4.6); so does this.
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
	go func() {
		served <- lb.pool.ServeSpoke(ctx, link, hub, ShareSide(lv.rec.ID, alpha.inst.KubeKey, share), nil)
	}()
	if err := os.WriteFile(filepath.Join(host, ".claude", "from-host.md"), []byte("written on the host"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(beta, "echo from-beta > /opt/data/pod.txt && head -c 600000 /dev/zero | tr '\\0' b > ~/.claude/big.bin")
	flushLive(t, ctx, hub)
	if got := sh(beta, "cat ~/.claude/from-host.md"); got != "written on the host" {
		t.Errorf("a host write is %q in the pods", got)
	}
	if data, _ := os.ReadFile(filepath.Join(host, ".mnt/opt/data/pod.txt")); string(data) != "from-beta\n" {
		t.Errorf("a pod's write is %q on the host", data)
	}
	if fi, err := os.Stat(filepath.Join(host, ".claude/big.bin")); err != nil || fi.Size() != 600000 {
		t.Errorf("a pod's large file on the host: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(filepath.Join(host, "files/.claude.json")); string(data) != "{}" {
		t.Errorf("the host's .claude.json is %q", data)
	}
	_ = link.Close()
	select {
	case <-served:
	case <-time.After(30 * time.Second):
		t.Error("the spoke's connection outlived its close")
	}

	// A stop of both and a start of one keep the share.
	for _, m := range []*made{alpha, beta} {
		if err := m.b.Stop(m.res.ContainerID); err != nil {
			t.Fatalf("stop %s: %v", m.inst.Name, err)
		}
	}
	if err := beta.b.Start(beta.res.ContainerID); err != nil {
		t.Fatalf("start beta: %v", err)
	}
	if got := sh(beta, "cat ~/.claude/shared.txt ~/.claude/from-host.md"); got != "from-alpha\nwritten on the host" {
		t.Errorf("after a stop and start: %q", got)
	}

	for name, m := range got {
		if err := m.b.Down(m.res.ContainerID); err != nil {
			t.Errorf("down %s: %v", name, err)
		}
		lb.waitNothingLeft(t, lv, m.inst.KubeKey)
	}
	got = nil
	if _, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Get(ctx, claim, metav1.GetOptions{}); err != nil {
		t.Errorf("the fleet's share went with its instances: %v", err)
	}
}

// classifyStorage puts the cluster's StorageClasses into lv's facts, as the
// preflight does.
func (lb *liveBackend) classifyStorage(t *testing.T, lv *live) {
	t.Helper()
	list, err := lv.cs.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]*storagev1.CSIDriver{}
	if dl, err := lv.cs.StorageV1().CSIDrivers().List(context.Background(), metav1.ListOptions{}); err == nil {
		for i := range dl.Items {
			drivers[dl.Items[i].Name] = &dl.Items[i]
		}
	}
	// What preflight.ClassifyStorageClass says of them (which this package
	// cannot import): their binding, whether they are a node's disk, and
	// SELinux mounts.
	var classes []*fleetgrpc.KubeStorageClass
	for _, sc := range list.Items {
		ks := &fleetgrpc.KubeStorageClass{
			Name: sc.Name, Provisioner: sc.Provisioner, Block: true,
			IsDefault:   sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true",
			NodeLocal:   strings.Contains(sc.Provisioner, "hostpath") || strings.Contains(sc.Provisioner, "local"),
			BindingMode: string(storagev1.VolumeBindingImmediate),
		}
		if sc.VolumeBindingMode != nil {
			ks.BindingMode = string(*sc.VolumeBindingMode)
		}
		if d := drivers[sc.Provisioner]; d != nil && d.Spec.SELinuxMount != nil {
			ks.SelinuxMount = *d.Spec.SELinuxMount
		}
		classes = append(classes, ks)
	}
	v, _ := lv.cs.Discovery().ServerVersion()
	lb.mu.Lock()
	f := lb.facts[lv.rec.ID]
	f.Capabilities.StorageClasses = classes
	if v != nil {
		f.Capabilities.Version = v.GitVersion
	}
	lb.facts[lv.rec.ID] = f
	lb.mu.Unlock()
}

func flushLive(t *testing.T, ctx context.Context, hub *sharesync.Hub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := hub.Flush(fctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			st, _ := json.Marshal(hub.Status())
			t.Fatalf("Flush: %v\n%s", err, st)
		}
	}
}

func ptrOr(p *string) string {
	if p == nil {
		return "(default)"
	}
	return *p
}
