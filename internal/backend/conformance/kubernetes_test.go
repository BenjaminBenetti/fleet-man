//go:build !nok8s

package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/backendtest"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/server"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func init() {
	targets[backendtest.BackendKubernetes] = kubernetesTarget
}

// envConformanceStorageClass names the StorageClass of the run's instance
// disks; unset leaves it to the connection's auto pick.
const envConformanceStorageClass = "FLEET_CONFORMANCE_KUBE_STORAGE_CLASS"

// envConformanceSnapshotClass sets the connection's snapshot_class, and
// envConformanceCloneConsistency its clone_consistency.
const (
	envConformanceSnapshotClass    = "FLEET_CONFORMANCE_KUBE_SNAPSHOT_CLASS"
	envConformanceCloneConsistency = "FLEET_CONFORMANCE_KUBE_CLONE_CONSISTENCY"
)

// kubernetesTarget is the kubernetes backend as fleetd binds it, on a
// connection to the selected kube context (the current one when none is
// named) in a namespace of its own, fleet-it-conformance-<random>, deleted
// when the run ends. A Kubernetes instance lives in fleetd's state (its
// record, its connection, the daemon's pool, preflight facts, image delivery
// and the PodExec its exec argvs dial), so the target runs fleetd itself,
// server.Serve, in this process under the run's HOME, and binds each
// instance through backendutil.NewForInstance after saving its record, as a
// create job does. It builds the fleet binary its argvs run and the pod build
// fleetd stages into pods; FLEET_POD_TMUX_<ARCH> names the tmux bundle for
// images without tmux (the fixture has its own).
func kubernetesTarget(t *testing.T, sel backendtest.Selection) backendtest.Target {
	for _, tool := range []string{"docker", "devcontainer", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the kubernetes conformance run needs %s on PATH: %v", tool, err)
		}
	}
	bins := t.TempDir()
	exe := buildFleet(t, filepath.Join(bins, "fleet"), nil)
	// The daemon here is this test binary, which records no VCS state: the
	// pod build records none either, so the pool serves it as the daemon's
	// own build.
	pod := buildFleet(t, filepath.Join(bins, binstore.BinaryAsset(runtime.GOARCH)), []string{"-tags", "nok8s", "-buildvcs=false"}, "GOOS=linux")
	t.Setenv(binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH), pod)
	kubernetes.SetFleetExecutable(exe)
	t.Cleanup(func() { kubernetes.SetFleetExecutable("") })

	paths := kubeexec.DefaultKubeconfigPaths(os.Environ())
	kctx := sel.Qualifier
	if kctx == "" {
		current, err := kubeexec.CurrentContext(paths)
		if err != nil || current == "" {
			t.Fatalf("FLEET_CONFORMANCE=kubernetes names no context and the kubeconfig has no current one (%v)", err)
		}
		kctx = current
	}
	rec := fleet.KubeCluster{
		ID: names.NewClusterID("conformance", nil), Context: kctx, KubeconfigPaths: paths,
		Namespace: "fleet-it-conformance-" + names.NewClusterID("x", nil)[2:],
		// The instance disks' class picks the clone tiers the run covers:
		// a class with snapshots (csi-hostpath-sc) clones by snapshot and CSI
		// clone, one without (minikube's standard) by copying.
		StorageClass: os.Getenv(envConformanceStorageClass),
		// A snapshot class that does not exist leaves a stopped instance's
		// clone to a CSI clone (and its fallback to a copy).
		SnapshotClass: os.Getenv(envConformanceSnapshotClass),
		// stop-source has a clone stop its running source for the final copy.
		CloneConsistency: fleet.KubeCloneConsistency(os.Getenv(envConformanceCloneConsistency)),
	}
	conn, err := kubeexec.Load(rec, kubeexec.Options{})
	if err != nil {
		t.Fatalf("load context %s: %v", kctx, err)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rec.Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", rec.Namespace, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), rec.Namespace, metav1.DeleteOptions{})
	})
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = append(k.Clusters, rec)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	startDaemon(t)
	id, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}

	return backendtest.Target{
		Name: backendtest.BackendKubernetes + ":" + kctx,
		Type: fleet.BackendKubernetes,
		Bind: func(inst *fleet.Instance) backend.Backend {
			if inst.KubeCluster == "" {
				inst.KubeCluster = rec.ID
			}
			if inst.KubeKey == "" {
				inst.KubeKey = names.InstanceKey(id.DaemonID, backendtest.FleetName, inst.Name, inst.CreatedAt)
			}
			saveRecord(t, inst)
			return backendutil.NewForInstance(inst, false)
		},
		Capabilities: backendutil.CapabilitiesFor,
		Leftovers: func(inst *fleet.Instance) ([]string, error) {
			return kubeLeftovers(rec.ID, inst)
		},
		EventLog:        flog.Path(),
		Sizes:           backendtest.PlanSizes(),
		ListSessionsP95: 15 * time.Millisecond,
		Skip: map[string]string{
			"up-again":        "a cluster create always gets a key of its own: the daemon never runs a second Up on one, so there is nothing to adopt or prune",
			"forward":         "port forwarding to a pod lands with P3.3 (PortDialer)",
			"editor-uri":      "the editor launch for a pod lands with P7.0 (Remote-SSH)",
			"rebuild":         "rebuild on a cluster lands with P6.1",
			"rebuild-stopped": "rebuild on a cluster lands with P6.1",
		},
	}
}

// buildFleet builds the fleet command to out with extra go build flags and
// environment.
func buildFleet(t *testing.T, out string, flags []string, env ...string) string {
	t.Helper()
	args := append([]string{"build"}, flags...)
	args = append(args, "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd := exec.Command("go", args...)
	cmd.Env = append(append(os.Environ(), "CGO_ENABLED=0"), env...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, msg)
	}
	return out
}

// startDaemon runs fleetd in this process until the run ends, and waits for
// its socket.
func startDaemon(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("fleetd: %v", err)
			}
		case <-time.After(time.Minute):
			t.Log("fleetd did not stop within a minute")
		}
	})
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(fleetpaths.SocketPath()); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("fleetd exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fleetd never made its socket")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// saveRecord puts inst into state.json's conformance fleet, as a create job
// keeps the instance's record (fleetd finds the instance's fleet, watches its
// pod and serves its PodExecs from it).
func saveRecord(t *testing.T, inst *fleet.Instance) {
	t.Helper()
	cp := *inst
	if err := state.Update(func(st *state.State) error {
		f := st.GetOrCreateFleet(backendtest.FleetName, "https://example.invalid/conformance.git")
		for i, cur := range f.Instances {
			if cur.Name == cp.Name {
				f.Instances[i] = &cp
				return nil
			}
		}
		return f.AddInstance(&cp)
	}); err != nil {
		t.Fatalf("save %s: %v", inst.Name, err)
	}
}

// kubeLeftovers lists what the connection still holds labelled with inst's
// key ("<kind> <name>"); once nothing is left the record goes from
// state.json too, as a destroy removes it.
func kubeLeftovers(clusterID string, inst *fleet.Instance) ([]string, error) {
	if inst.KubeKey == "" {
		return nil, nil
	}
	scope, err := kubernetes.DefaultPool().Scope(clusterID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objs, err := scope.ListByKey(ctx, inst.KubeKey)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, o := range objs {
		held = append(held, o.Kind+" "+o.Name)
	}
	if len(held) == 0 {
		err := state.Update(func(st *state.State) error {
			if f, ok := st.Fleets[backendtest.FleetName]; ok {
				_ = f.RemoveInstance(inst.Name)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("drop the record: %w", err)
		}
	}
	return held, nil
}
