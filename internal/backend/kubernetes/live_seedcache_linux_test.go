//go:build !nok8s && linux

package kubernetes

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveSeedCache runs automation creates on a connection whose instance
// disks are on a class that clones (FLEET_KUBE_TEST_CLONE_CLASS, by default
// csi-hostpath-sc, minikube's csi-hostpath-driver addon): the first keeps
// its seeded root as the image's seed cache, the second's disk is cloned
// from that cache and its seed keeps the cached root, both instances work,
// destroying them leaves the cache (it is the connection's), and removing
// the connection's caches deletes it. It needs what TestLiveBackendLifecycle
// does; its namespaces are fleet-it-p6-02a-seedcache-*.
func TestLiveSeedCache(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run instances on a cluster")
	}
	for _, tool := range []string{"docker", "devcontainer"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	class := os.Getenv("FLEET_KUBE_TEST_CLONE_CLASS")
	if class == "" {
		class = "csi-hostpath-sc"
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	lb := newLiveBackend(t, kctx)
	lv := newLiveIn(t, lb.kctx, "fleet-it-p6-02a-seedcache-")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := lv.cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{}); err != nil {
		t.Skipf("no storage class %s to clone disks on: %v", class, err)
	}
	lv.rec.StorageClass = class
	f := lv.facts(t)
	f.Capabilities.CloneTier = "snapshot"
	lb.mu.Lock()
	lb.recs = append(lb.recs, lv.rec)
	lb.facts[lv.rec.ID] = f
	recs := append([]fleet.KubeCluster(nil), lb.recs...)
	lb.mu.Unlock()
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = recs
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lb.pool.Configure(recs)
	write := func(ws string) {
		writeLiveWorkspace(t, ws, map[string]any{"image": image, "mounts": []string{"type=volume,target=/home/vscode/.cache"}})
	}

	create := func(name string) (*fleet.Instance, *backend.UpResult, time.Duration) {
		t.Helper()
		inst, _ := lb.instance(t, lv, name, write)
		inst.Automated = true
		start := time.Now()
		res, err := newHandle(inst, lb.pool).UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		took := time.Since(start)
		t.Logf("created %s in %v", res.ContainerID, took.Round(time.Second))
		inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
		return inst, res, took
	}
	check := func(inst *fleet.Instance, res *backend.UpResult) {
		t.Helper()
		out, err := newHandle(inst, lb.pool).RunScript(res.ContainerID, "cat "+res.RemoteWorkspaceFolder+"/README.md ~/.lifecycle; id -un; test -x /usr/bin/fleet && echo fleet-ok")
		if err != nil {
			t.Fatalf("%s: %v\n%s", inst.Name, err, out)
		}
		if got := strings.TrimSpace(out); got != "# live\ncreated\nstarted\nvscode\nfleet-ok" {
			t.Errorf("%s: %q", inst.Name, got)
		}
	}

	first, firstRes, firstTook := create("auto1")
	check(first, firstRes)
	caches, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).List(ctx, metav1.ListOptions{LabelSelector: names.LabelRole + "=" + string(names.RoleSeedCache)})
	if err != nil || len(caches.Items) != 1 {
		t.Fatalf("seed caches after the first automation create: %v, %v", caches, err)
	}
	cache := caches.Items[0]
	if cache.Annotations[names.AnnotationSeedCacheReady] == "" || cache.Labels[names.LabelKey] != "" || !ownSeedCache(cache.Labels, names.Owner(lb.daemonID), lv.rec.ID) {
		t.Fatalf("the seed cache %s: labels %v annotations %v", cache.Name, cache.Labels, cache.Annotations)
	}
	ix, err := LoadSeedCacheIndex(lv.rec.ID)
	if err != nil || ix.Find(cache.Name) == nil || ix.Find(cache.Name).Refs[0] != first.KubeKey {
		t.Fatalf("the index after the build: %+v, %v", ix, err)
	}

	second, secondRes, secondTook := create("auto2")
	check(second, secondRes)
	disk, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Get(ctx, names.Instance("live", "auto2", second.KubeKey), metav1.GetOptions{})
	if err != nil || disk.Spec.DataSource == nil || disk.Spec.DataSource.Name != cache.Name {
		t.Fatalf("the second disk's data source: %v, %v", disk.Spec.DataSource, err)
	}
	if log := seedJobLog(t, lv, names.SeedJob(names.Instance("live", "auto2", second.KubeKey), 1)); !strings.Contains(log, "root from seed cache") {
		t.Errorf("the second seed did not keep the cached root:\n%s", log)
	}
	t.Logf("the first create (seed + cache) took %v, the cloned one %v", firstTook.Round(time.Second), secondTook.Round(time.Second))
	if ix, _ = LoadSeedCacheIndex(lv.rec.ID); len(ix.Find(cache.Name).Refs) != 2 {
		t.Errorf("the index after the clone: %+v", ix.Find(cache.Name))
	}

	for _, inst := range []*fleet.Instance{first, second} {
		if err := newHandle(inst, lb.pool).Down(inst.ContainerID); err != nil {
			t.Fatalf("down %s: %v", inst.Name, err)
		}
		lb.waitNothingLeft(t, lv, inst.KubeKey)
	}
	if _, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Get(ctx, cache.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("destroying the instances took the seed cache: %v", err)
	}
	deleted, err := DeleteSeedCaches(ctx, lv.cs, lv.ns, names.Owner(lb.daemonID), lv.rec.ID)
	if err != nil || len(deleted) != 1 || deleted[0] != cache.Name {
		t.Fatalf("DeleteSeedCaches = %v, %v", deleted, err)
	}
}

// seedJobLog is the log of Job name's pod.
func seedJobLog(t *testing.T, lv *live, job string) string {
	t.Helper()
	pods, err := lv.cs.CoreV1().Pods(lv.ns).List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("the pods of Job %s: %v, %v", job, pods, err)
	}
	raw, err := lv.cs.CoreV1().Pods(lv.ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).DoRaw(context.Background())
	if err != nil {
		t.Fatalf("the log of Job %s: %v", job, err)
	}
	return string(bytes.TrimSpace(raw))
}
