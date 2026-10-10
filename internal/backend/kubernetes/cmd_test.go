//go:build !nok8s

package kubernetes

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// The exec seams hand out a self-contained `fleet kube-exec` argv that names
// the instance's pod, the connection, its kubeconfig and context, and the
// class, so any caller runs it unchanged.
func TestExecCommandBuildsTheKubeExecArgv(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)

	argv := b.ExecCommand("/ws", []string{"echo", "hi there"}).Args
	if filepathBase(argv[0]) == "" || argv[1] != KubeExecVerb {
		t.Fatalf("argv does not start with the fleet binary and kube-exec: %v", argv)
	}
	want := map[string]string{
		"--cluster": handleCluster,
		"-n":        handleNS,
		"--pod":     names.Pod(handleSTS()),
		"-c":        DevContainer,
		"--class":   "exec",
		"--context": "ctx",
	}
	for flag, val := range want {
		if got := flagValue(argv, flag); got != val {
			t.Errorf("%s = %q, want %q (argv %v)", flag, got, val, argv)
		}
	}
	if flagValue(argv, "--kubeconfig") == "" {
		t.Errorf("no --kubeconfig in %v", argv)
	}
	dash := indexOf(argv, "--")
	if dash < 0 || len(argv) < dash+3 || argv[dash+1] != "echo" || argv[dash+2] != "hi there" {
		t.Errorf("command after --: %v", argv)
	}
}

// LogsCommand builds the `fleet kube-exec logs` argv, carrying the fleet and
// instance (for the saved log) and -f when following.
func TestLogsCommandBuildsTheKubeExecLogsArgv(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	argv := b.LogsCommand(inst.ContainerID, true).Args
	if indexOf(argv, KubeExecVerb) < 0 || indexOf(argv, "logs") < 0 || indexOf(argv, "-f") < 0 {
		t.Fatalf("not a kube-exec logs -f argv: %v", argv)
	}
	if flagValue(argv, "--fleet") != "alpha" || flagValue(argv, "--instance") != "web" {
		t.Errorf("logs argv missing fleet/instance: %v", argv)
	}
}

// A handle whose record names no connection builds a command that fails with
// the reason when started, not a bare missing-program error.
func TestExecCommandWithoutAConnectionFailsClearly(t *testing.T) {
	r := newHandleRig(t)
	b := newHandle(&fleet.Instance{Name: "web", Backend: fleet.BackendKubernetes}, r.pool)
	err := b.ExecCommand("/ws", []string{"true"}).Run()
	if err == nil || !strings.Contains(err.Error(), "Kubernetes") {
		t.Fatalf("a handle with no connection: %v, want a clear reason", err)
	}
}

// FolderExecCommand routes a folder's tar through the bulk session: its
// kube-exec argv carries --bulk, where ExecCommand's does not.
func TestFolderExecCommandIsBulk(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	if argv := b.ExecCommand("/ws", []string{"tar"}).Args; indexOf(argv, "--bulk") >= 0 {
		t.Errorf("ExecCommand should not be bulk: %v", argv)
	}
	argv := b.FolderExecCommand("/ws", []string{"tar", "-cf", "-", "."}).Args
	if indexOf(argv, "--bulk") < 0 || indexOf(argv, "--") < 0 {
		t.Fatalf("FolderExecCommand argv not a bulk kube-exec: %v", argv)
	}
}

// peek reports a small reader's exact size and a large one as over the cap,
// and hands back all the bytes either way.
func TestPeekSizesTheInput(t *testing.T) {
	small := strings.Repeat("a", 100)
	rdr, size, err := peek(strings.NewReader(small), podwire.SmallFile)
	if err != nil || size != 100 {
		t.Fatalf("small: size %d err %v", size, err)
	}
	if got, _ := io.ReadAll(rdr); string(got) != small {
		t.Fatalf("small reader lost bytes: %d", len(got))
	}
	big := strings.Repeat("b", int(podwire.SmallFile)+500)
	rdr, size, err = peek(strings.NewReader(big), podwire.SmallFile)
	if err != nil || size != podwire.SmallFile+1 {
		t.Fatalf("large: size %d err %v, want over the cap", size, err)
	}
	if got, _ := io.ReadAll(rdr); string(got) != big {
		t.Fatalf("large reader lost bytes: %d of %d", len(got), len(big))
	}
}

func writeHandleCluster(t *testing.T) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kc")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: handleCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: handleNS}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func flagValue(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func indexOf(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

func filepathBase(p string) string { return filepath.Base(p) }
