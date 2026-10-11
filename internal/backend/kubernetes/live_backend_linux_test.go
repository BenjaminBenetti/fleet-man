//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/devcli"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveBackendLifecycle runs instances' whole lives through the backend
// handle against a real cluster: the create (capture-up with the real
// devcontainer CLI and docker, image delivery, the seed Job with the real pod
// build, the StatefulSet, verification, the agent, set-up's hooks), a stop
// and a start that keep what was written to the persisted root and a sibling
// /workspaces repo (postStart once per start), and a destroy that leaves no
// labelled object. It covers an image-only devcontainer (passthrough), a
// Dockerfile one (built on the daemon host, node-loaded on a local cluster),
// one in a restricted pod-security namespace (no root: one uid runs
// everything), and a create whose postCreateCommand fails (FAILED, scaled
// down, destroyed to nothing). It needs FLEET_KUBE_TEST_CONTEXT, docker, the
// devcontainer CLI and, for a local cluster, its tool (minikube); it builds
// the full and pod builds of fleet. Namespaces fleet-it-p2-12a-* are deleted
// when it ends. The image is the devcontainers base; FLEET_KUBE_TEST_IMAGE
// picks another.
func TestLiveBackendLifecycle(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run instances on a cluster")
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
	open := lb.connect(t, nil, true)

	t.Run("image", func(t *testing.T) {
		lb.lifecycle(t, open, "img", func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) }, "vscode")
	})
	t.Run("dockerfile", func(t *testing.T) {
		lb.lifecycle(t, open, "built", func(ws string) {
			writeLiveWorkspace(t, ws, map[string]any{"build": map[string]any{"dockerfile": "Dockerfile"}})
			if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "Dockerfile"), []byte("FROM "+image+"\nRUN echo built > /etc/fleet-live-built\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "vscode")
	})
	t.Run("restricted", func(t *testing.T) {
		restricted := lb.connect(t, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}, false)
		lb.lifecycle(t, restricted, "tierc", func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) }, "")
	})
	t.Run("group-add", func(t *testing.T) {
		lb.groupAdd(t, open, image)
	})
	t.Run("failed create", func(t *testing.T) {
		lb.failedCreate(t, open, image)
	})
	// The integration fixtures the P2 exit criteria name: a Dockerfile with
	// build args, a context above .devcontainer and a local feature; and the
	// go feature (whose debug grants an unlabelled namespace admits).
	t.Run("fixture-dockerfile", func(t *testing.T) {
		lb.fixture(t, open, "dockerfile", "fixture-dockerfile", map[string]string{
			"cat /etc/fleet-itest-build-arg":                  "from-build-args",
			"cat /etc/fleet-itest-context-readme | head -c 1": "#",
			"cat /usr/local/share/itest-feature-greeting":     "hello-from-feature-option",
			"itest-feature":                "itest-feature-ok",
			"printenv ITEST_CONTAINER_ENV": "from-containerEnv",
		})
	})
	t.Run("fixture-go-feature", func(t *testing.T) {
		lb.fixture(t, open, "gofeature", "fixture-go-feature", map[string]string{
			"/usr/local/go/bin/go version | cut -d' ' -f3 | cut -c1-6": "go1.24",
		})
	})
	t.Run("exec-surface", func(t *testing.T) {
		lb.execSurface(t, open, image)
	})
	t.Run("sessions", func(t *testing.T) {
		restricted := lb.connect(t, map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}, false)
		lb.sessions(t, restricted, "tiercsess", image)
	})
}

// execSurface exercises the backend's exec surface on a real pod: OpenExec
// runs a command and reports its exit code and streams apart, and CopyFile
// streams a file into the pod as the remote user.
func (lb *liveBackend) execSurface(t *testing.T, lv *live, image string) {
	inst, _ := lb.instance(t, lv, "exec", func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) })
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
	b = newHandle(inst, lb.pool)
	defer func() {
		if err := b.Down(res.ContainerID); err != nil {
			t.Errorf("down: %v", err)
		}
		lb.waitNothingLeft(t, lv, inst.KubeKey)
	}()

	stream, err := b.OpenExec(ctx, backend.ExecRequest{Class: backend.ExecClassExec, ContainerID: res.ContainerID, Argv: []string{"sh", "-c", "printf out; printf err >&2; exit 4"}})
	if err != nil {
		t.Fatalf("OpenExec: %v", err)
	}
	var out, errb strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, stream.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errb, stream.Stderr()) }()
	_ = stream.Stdin().Close()
	code, werr := stream.Wait()
	wg.Wait()
	if werr != nil || code != 4 || out.String() != "out" || errb.String() != "err" {
		t.Errorf("OpenExec: code %d err %v stdout %q stderr %q", code, werr, out.String(), errb.String())
	}

	payload := "copied-in\n"
	if err := b.CopyFile(inst.WorkspaceDir, strings.NewReader(payload), "/tmp/fleet-copy-test", 0o644); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if got, err := b.RunScript(res.ContainerID, "cat /tmp/fleet-copy-test"); err != nil || got != payload {
		t.Errorf("the copied file reads %q (%v), want %q", got, err, payload)
	}

	// The workspace is read in the pod (WorkspaceReader), where a branch
	// switched inside the instance is.
	// As the remote user (the exec class), whose checkout it is.
	gitInit, err := b.OpenExec(ctx, backend.ExecRequest{Class: backend.ExecClassExec, ContainerID: res.ContainerID,
		Argv: []string{"sh", "-c", "cd " + res.RemoteWorkspaceFolder + " && git init -q -b live-branch && printf '{}' > .devcontainer/extra.json"}})
	if err != nil {
		t.Fatalf("OpenExec: %v", err)
	}
	_ = gitInit.Stdin().Close()
	go func() { _, _ = io.Copy(io.Discard, gitInit.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, gitInit.Stderr()) }()
	if code, err := gitInit.Wait(); err != nil || code != 0 {
		t.Fatalf("git init in the pod: code %d %v", code, err)
	}
	if branch, ok := b.Branch(ctx, inst.WorkspaceDir); !ok || branch != "live-branch" {
		t.Errorf("Branch = %q %v, want live-branch from the pod", branch, ok)
	}
	if data, err := b.ReadWorkspaceFile(ctx, inst.WorkspaceDir, ".devcontainer/extra.json"); err != nil || string(data) != "{}" {
		t.Errorf("ReadWorkspaceFile of a file made in the pod = %q %v", data, err)
	}
}

// sessions proves a tmux-less image gets tmux sessions through the bundled
// static tmux, with nothing run from a package mirror, and the session list
// shows them (twouser).
func (lb *liveBackend) sessions(t *testing.T, lv *live, name, image string) {
	if os.Getenv(binstore.EnvTmuxPrefix+strings.ToUpper(runtime.GOARCH)) == "" {
		t.Skipf("set %s%s=<fleet-tmux-linux-%s.tar.gz> (build with `make pod-tmux`) to test the bundled tmux", binstore.EnvTmuxPrefix, strings.ToUpper(runtime.GOARCH), runtime.GOARCH)
	}
	inst, _ := lb.instance(t, lv, name, func(ws string) { writeLiveWorkspace(t, ws, map[string]any{"image": image}) })
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
	b = newHandle(inst, lb.pool)
	defer func() {
		if err := b.Down(res.ContainerID); err != nil {
			t.Errorf("down: %v", err)
		}
		lb.waitNothingLeft(t, lv, inst.KubeKey)
	}()

	if err := b.EnsureTmux(ctx); err != nil {
		t.Fatalf("EnsureTmux: %v", err)
	}
	if _, err := b.RunScript(res.ContainerID, "tmux new-session -d -s one 'sleep 600'; tmux new-session -d -s two 'sleep 600'"); err != nil {
		t.Fatalf("create tmux sessions through the bundle: %v", err)
	}
	list := b.ListSessions(res.ContainerID)
	if !strings.Contains(list, "one:") || !strings.Contains(list, "two:") {
		t.Errorf("ListSessions %q, want sessions one and two", list)
	}
	if out, _ := b.RunScript(res.ContainerID, "cat /var/log/apt/history.log 2>/dev/null || echo no-apt"); strings.Contains(out, "tmux") {
		t.Errorf("tmux looks apt-installed, not bundled:\n%s", out)
	}
}

// groupAdd: a runArgs --group-add naming a group of the image becomes its
// gid in the pod's supplementalGroups.
func (lb *liveBackend) groupAdd(t *testing.T, lv *live, image string) {
	inst, sts := lb.instance(t, lv, "groups", func(ws string) {
		writeLiveWorkspace(t, ws, map[string]any{"image": image, "runArgs": []string{"--group-add", "users"}})
	})
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	set, err := lv.cs.AppsV1().StatefulSets(lv.ns).Get(ctx, sts, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sc := set.Spec.Template.Spec.SecurityContext; sc == nil || !slices.Contains(sc.SupplementalGroups, 100) {
		t.Errorf("the pod's security context %+v, want the image's users group (100) in its supplementalGroups", sc)
	}
	if err := b.Down(res.ContainerID); err != nil {
		t.Fatalf("down: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

// fixture creates an instance from an integration fixture, checks what the
// scripts print, and destroys it.
func (lb *liveBackend) fixture(t *testing.T, lv *live, name, fixture string, checks map[string]string) {
	src := filepath.Join("..", "..", "..", "integration", fixture)
	inst, _ := lb.instance(t, lv, name, func(ws string) {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("cp", "-a", src+"/.", ws).CombinedOutput(); err != nil {
			t.Fatalf("copying %s: %v\n%s", fixture, err, out)
		}
	})
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create from %s: %v", fixture, err)
	}
	t.Logf("created %s from %s in %v", res.ContainerID, fixture, time.Since(start).Round(time.Second))
	inst.ContainerID = res.ContainerID
	b = newHandle(inst, lb.pool)
	for script, want := range checks {
		out, err := b.RunScript(res.ContainerID, script)
		if got := strings.TrimSpace(out); err != nil || got != want {
			t.Errorf("%q = %q (%v), want %q", script, got, err, want)
		}
	}
	if err := b.Down(res.ContainerID); err != nil {
		t.Fatalf("down: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

// liveBackend is the daemon side of the live tests: the pool, the
// environment and the connections.
type liveBackend struct {
	kctx     string
	pool     *Pool
	daemonID string
	// prefix starts the names of the namespaces connect makes.
	prefix string

	mu      sync.Mutex
	recs    []fleet.KubeCluster
	facts   map[string]Facts
	tracked []Tracked
}

func newLiveBackend(t *testing.T, kctx string) *liveBackend {
	realHome, _ := os.UserHomeDir()
	if os.Getenv("MINIKUBE_HOME") == "" {
		t.Setenv("MINIKUBE_HOME", filepath.Join(realHome, ".minikube"))
	}
	// The binaries are built before HOME moves (the go build cache is the
	// real one).
	bins := &live{t: t, dir: t.TempDir()}
	art := bins.buildPod("vlive-12a")
	exe := buildFullFleet(t, bins.dir)
	lb := &liveBackend{kctx: kctx, facts: map[string]Facts{}}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", filepath.Join(realHome, ".docker"))
	id, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lb.daemonID = id.DaemonID
	lb.pool = NewPool(PoolOptions{
		Records: func() ([]fleet.KubeCluster, error) {
			lb.mu.Lock()
			defer lb.mu.Unlock()
			return append([]fleet.KubeCluster(nil), lb.recs...), nil
		},
		// The pod binary is served from the store dir; a tmux bundle, when
		// one is built, comes from FLEET_POD_TMUX_<ARCH> in the real
		// environment (the sessions subtest needs it and skips without it).
		Bins: &binstore.Store{Dir: filepath.Dir(art.Path), Self: art.Info.Identity, Getenv: func(k string) string {
			if strings.HasPrefix(k, binstore.EnvTmuxPrefix) {
				return os.Getenv(k)
			}
			return ""
		}},
		Version: art.Info.Version,
		Logger:  slog.New(slog.NewTextHandler(testLog{t}, nil)),
	})
	t.Cleanup(lb.pool.Close)
	images := &kubeimage.Manager{IndexPath: state.KubeImagesPath}
	SetEnvironment(Environment{
		Images: images,
		Facts: func(_ context.Context, id string) (Facts, error) {
			lb.mu.Lock()
			defer lb.mu.Unlock()
			f, ok := lb.facts[id]
			if !ok {
				return Facts{}, kubeerr.New(kubeerr.ClusterMissing, "no connection %s", id)
			}
			return f, nil
		},
		Tools: devcli.Tools{Exe: exe},
	})
	t.Cleanup(func() { SetEnvironment(Environment{}) })
	// The run's daemon id is its own, so no later GC would ever find the
	// images it delivered: they go when it ends.
	t.Cleanup(func() { lb.collectImages(t, images) })
	return lb
}

// collectImages collects every image the run delivered to its connections,
// from the nodes and the daemon host's docker, with the pass fleetd makes
// over a connection it removes (no holder left, no retention), then checks
// that no node load tag of the run's daemon is left on the host.
func (lb *liveBackend) collectImages(t *testing.T, images *kubeimage.Manager) {
	lb.mu.Lock()
	recs := append([]fleet.KubeCluster(nil), lb.recs...)
	lb.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for i := range recs {
		rec := recs[i]
		if _, err := os.Stat(state.KubeImagesPath(rec.ID)); err != nil {
			continue // nothing was delivered there
		}
		res, err := images.GC(ctx, kubeimage.Target{Cluster: &rec, DaemonID: lb.daemonID}, kubeimage.GCOptions{All: true, Live: func(string) bool { return false }})
		if err != nil {
			t.Errorf("collect the images delivered to %s: %v", rec.ID, err)
			continue
		}
		for _, e := range res.Kept {
			t.Errorf("image %s on %s was not collected: %s", e.Ref, rec.ID, e.GCError)
		}
	}
	if left := hostImageTags(t, kubeimage.NodeLoadRepository(lb.daemonID)); len(left) > 0 {
		t.Errorf("the run left images on the host's docker: %v", left)
	}
}

// hostImageTags lists the host docker's tags in repository repo.
func hostImageTags(t *testing.T, repo string) []string {
	t.Helper()
	out, err := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference="+repo).Output()
	if err != nil {
		t.Errorf("list the host's %s images: %v", repo, err)
		return nil
	}
	return strings.Fields(string(out))
}

// connect adds a connection to a fresh namespace with labels, whose facts
// say whether root pods are admitted, its record changed by edits.
func (lb *liveBackend) connect(t *testing.T, labels map[string]string, rootAdmitted bool, edits ...func(*fleet.KubeCluster)) *live {
	t.Helper()
	prefix := lb.prefix
	if prefix == "" {
		prefix = "fleet-it-p2-12a-k8s-backend-"
	}
	return lb.connectIn(t, prefix, labels, rootAdmitted, edits...)
}

// connectIn is connect to a namespace named with prefix.
func (lb *liveBackend) connectIn(t *testing.T, prefix string, labels map[string]string, rootAdmitted bool, edits ...func(*fleet.KubeCluster)) *live {
	t.Helper()
	lv := newLiveIn(t, lb.kctx, prefix)
	for _, edit := range edits {
		edit(&lv.rec)
	}
	if len(labels) > 0 {
		ns, err := lv.cs.CoreV1().Namespaces().Get(context.Background(), lv.ns, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range labels {
			ns.Labels[k] = v
		}
		if _, err := lv.cs.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f := lv.facts(t)
	f.Capabilities.Admission.Unprivileged = rootAdmitted
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
	return lv
}

// instance makes a CREATING record of instance name on lv's connection,
// with its workspace written by write.
func (lb *liveBackend) instance(t *testing.T, lv *live, name string, write func(ws string)) (*fleet.Instance, string) {
	t.Helper()
	fleetName := "live"
	ws := filepath.Join(state.WorkspacesDir(), fleetName, name, fleetName)
	write(ws)
	inst := &fleet.Instance{
		Name: name, Backend: fleet.BackendKubernetes, KubeCluster: lv.rec.ID, WorkspaceDir: ws,
		CreatedAt: time.Now().UTC(), Status: fleet.StatusCreating,
	}
	inst.KubeKey = names.InstanceKey(lb.daemonID, fleetName, name, inst.CreatedAt)
	if err := state.Update(func(st *state.State) error {
		return st.GetOrCreateFleet(fleetName, "https://example.invalid/live.git").AddInstance(inst)
	}); err != nil {
		t.Fatal(err)
	}
	sts := names.Instance(fleetName, name, inst.KubeKey)
	lb.mu.Lock()
	lb.tracked = append(lb.tracked, Tracked{Cluster: lv.rec.ID, StatefulSet: sts})
	tracked := append([]Tracked(nil), lb.tracked...)
	lb.mu.Unlock()
	// The daemon's kubePods watches every Kubernetes record.
	lb.pool.Track(tracked)
	return inst, sts
}

// lifecycle creates, stops, starts and destroys one instance. user is the
// user its scripts run as ("" for a pod without root, whose one uid runs
// them).
func (lb *liveBackend) lifecycle(t *testing.T, lv *live, name string, write func(ws string), user string) {
	inst, sts := lb.instance(t, lv, name, write)
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created %s in %v (remote user %s, folder %s)", res.ContainerID, time.Since(start).Round(time.Second), res.RemoteUser, res.RemoteWorkspaceFolder)
	if want := ContainerID(lv.rec.ID, lv.ns, sts, 1); res.ContainerID != want {
		t.Fatalf("ContainerID %s, want %s", res.ContainerID, want)
	}
	if _, err := os.Stat(JournalPath(inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the create's journal is still there: %v", err)
	}
	rec, err := LoadInstanceRecord(lv.rec.ID, inst.KubeKey)
	if err != nil {
		t.Fatalf("instance record: %v", err)
	}
	t.Logf("delivered by %s as %s; the pod may run %v", rec.Strategy, rec.Image, rec.ImageDigests)
	inst.ContainerID, inst.Status = res.ContainerID, fleet.StatusRunning
	b = newHandle(inst, lb.pool)

	sh := func(script string) string {
		t.Helper()
		out, err := b.RunScript(res.ContainerID, script)
		if err != nil {
			t.Fatalf("%q: %v\n%s", script, err, out)
		}
		return strings.TrimSpace(out)
	}
	if user != "" {
		if got := sh("id -un"); got != user {
			t.Errorf("scripts run as %q, want %s", got, user)
		}
	} else if got := sh("id -u"); got == "0" {
		t.Errorf("a pod without root runs scripts as root")
	}
	if got := sh("cat " + res.RemoteWorkspaceFolder + "/README.md"); got != "# live" {
		t.Errorf("the workspace's README: %q", got)
	}
	if got := sh("cat ~/.lifecycle"); got != "created\nstarted" {
		t.Errorf("hooks after the create: %q", got)
	}
	if user != "" {
		// /workspaces is root's, as the workspace mount's parent is on
		// docker.
		sh("sudo sh -c 'echo persisted > /usr/local/bin/fleet-live-marker && mkdir /workspaces/sibling && chown " + user + ": /workspaces/sibling' && echo sib > /workspaces/sibling/file")
		if os.Getenv("FLEET_KUBE_TEST_NO_APT") == "" {
			// A package apt installs (its binary in /usr/bin) is the
			// persisted root's too.
			sh("sudo sh -c 'apt-get update -qq >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends tree >/dev/null' && tree --version")
		}
	} else {
		sh("echo persisted > ~/fleet-live-marker")
	}

	if err := b.Stop(res.ContainerID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	set, err := lv.cs.AppsV1().StatefulSets(lv.ns).Get(ctx, sts, metav1.GetOptions{})
	if err != nil || *set.Spec.Replicas != 0 {
		t.Fatalf("after the stop: %v, %v", set.Spec.Replicas, err)
	}
	if _, err := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, names.Pod(sts), metav1.GetOptions{}); err == nil {
		t.Fatal("the pod is still there after the stop")
	}
	if err := b.Start(res.ContainerID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if user != "" {
		if got := sh("cat /usr/local/bin/fleet-live-marker /workspaces/sibling/file"); got != "persisted\nsib" {
			t.Errorf("after stop and start: %q", got)
		}
		if os.Getenv("FLEET_KUBE_TEST_NO_APT") == "" {
			if got := sh("command -v tree && dpkg-query -W -f '${Status}' tree"); got != "/usr/bin/tree\ninstall ok installed" {
				t.Errorf("the apt-installed package after stop and start: %q", got)
			}
		}
	} else if got := sh("cat ~/fleet-live-marker"); got != "persisted" {
		t.Errorf("after stop and start: %q", got)
	}
	if got := sh("cat ~/.lifecycle"); got != "created\nstarted\nstarted" {
		t.Errorf("hooks after a stop and start: %q (postStart once per start)", got)
	}
	if obs := lb.pool.Observe(lv.rec.ID, sts); obs.Live != backend.LiveStatusRunning {
		t.Errorf("status after the start: %+v", obs)
	}

	if err := b.Down(res.ContainerID); err != nil {
		t.Fatalf("down: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
	if _, err := os.Stat(InstanceRecordPath(lv.rec.ID, inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the instance record outlived the instance: %v", err)
	}
}

// failedCreate: a postCreateCommand that fails fails the create after the
// seed, its pod scaled to 0 and its objects kept by key; the destroy then
// leaves nothing.
func (lb *liveBackend) failedCreate(t *testing.T, lv *live, image string) {
	inst, sts := lb.instance(t, lv, "broken", func(ws string) {
		writeLiveWorkspace(t, ws, map[string]any{"image": image, "postCreateCommand": "echo the hook fails >&2; exit 3"})
	})
	b := newHandle(inst, lb.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	_, err := b.UpContext(backend.WithJobLog(ctx, &lineLog{t: t}), backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if err == nil || !strings.Contains(err.Error(), "lifecycle hooks") {
		t.Fatalf("a create whose postCreateCommand fails: %v", err)
	}
	t.Logf("failed as expected: %v", err)
	set, err := lv.cs.AppsV1().StatefulSets(lv.ns).Get(ctx, sts, metav1.GetOptions{})
	if err != nil || set.Spec.Replicas == nil || *set.Spec.Replicas != 0 {
		t.Fatalf("the failed instance's StatefulSet: %v, %v", set, err)
	}
	if _, err := os.Stat(JournalPath(inst.KubeKey)); !os.IsNotExist(err) {
		t.Errorf("the failed create's journal is still there: %v", err)
	}
	// The destroy of the FAILED record, which has no ContainerID.
	if err := b.DownByKey(); err != nil {
		t.Fatalf("down by key: %v", err)
	}
	lb.waitNothingLeft(t, lv, inst.KubeKey)
}

func (lb *liveBackend) waitNothingLeft(t *testing.T, lv *live, key string) {
	t.Helper()
	scope, err := lb.pool.Scope(lv.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		left, err := scope.ListByKey(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("objects of the destroyed instance: %v", left)
		}
		time.Sleep(2 * time.Second)
	}
}

// writeLiveWorkspace writes a git-less checkout whose devcontainer (cfg,
// plus a remote user and hooks that count in ~/.lifecycle) the create runs.
func writeLiveWorkspace(t *testing.T, ws string, cfg map[string]any) {
	t.Helper()
	full := map[string]any{
		"remoteUser":        "vscode",
		"postCreateCommand": "echo created >> ~/.lifecycle",
		"postStartCommand":  "echo started >> ~/.lifecycle",
	}
	for k, v := range cfg {
		full[k] = v
	}
	data, _ := json.Marshal(full)
	if err := os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".devcontainer", "devcontainer.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildFullFleet builds the full fleet binary (the docker shim capture-up
// and set-up run through).
func buildFullFleet(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "full", "fleet")
	cmd := exec.Command("go", "build", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fleet: %v\n%s", err, msg)
	}
	return out
}

// facts is the connection's facts as a preflight would find them: its
// nodes, whether they are a local cluster, root pods admitted.
func (lv *live) facts(t *testing.T) Facts {
	t.Helper()
	nodes, err := lv.cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var lnodes []localcluster.Node
	caps := &fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}
	for _, n := range nodes.Items {
		lnodes = append(lnodes, localcluster.Node{Name: n.Name, Labels: n.Labels, ProviderID: n.Spec.ProviderID})
		caps.Nodes = append(caps.Nodes, &fleetgrpc.KubeNode{Name: n.Name, Arch: n.Status.NodeInfo.Architecture,
			AllocatableCpu: n.Status.Allocatable.Cpu().String(), AllocatableMemory: n.Status.Allocatable.Memory().String()})
	}
	local := localcluster.Detect(lnodes)
	caps.LocalCluster = local.Local()
	return Facts{Local: local, Platform: "linux/" + runtime.GOARCH, Capabilities: caps}
}

// lineLog is a job log that goes to the test's log, line by line (the
// devcontainer CLI's stack-trace lines left out).
type lineLog struct {
	t   *testing.T
	mu  sync.Mutex
	buf []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		if line := string(l.buf[:i]); !strings.HasPrefix(strings.TrimSpace(line), "at ") {
			l.t.Log("job: " + line)
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}
