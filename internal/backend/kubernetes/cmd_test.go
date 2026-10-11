//go:build !nok8s

package kubernetes

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
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

// A process that is not the fleet binary (the conformance suite's test
// binary running fleetd) names the one to hand out: the argvs and the
// capture shim run it, and "" puts this process's own back.
func TestSetFleetExecutable(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	SetFleetExecutable("/opt/itest/fleet")
	t.Cleanup(func() { SetFleetExecutable("") })
	if argv := b.ExecCommand("/ws", []string{"true"}).Args; argv[0] != "/opt/itest/fleet" {
		t.Errorf("ExecCommand runs %q", argv[0])
	}
	if argv := b.LogsCommand(inst.ContainerID, false).Args; argv[0] != "/opt/itest/fleet" {
		t.Errorf("LogsCommand runs %q", argv[0])
	}
	if exe := currentEnvironment().Tools.Exe; exe != "/opt/itest/fleet" {
		t.Errorf("the capture shim runs %q", exe)
	}
	SetFleetExecutable("")
	self, _ := os.Executable()
	if argv := b.ExecCommand("/ws", []string{"true"}).Args; argv[0] != self {
		t.Errorf("after the reset ExecCommand runs %q, want %q", argv[0], self)
	}
	if exe := currentEnvironment().Tools.Exe; exe != "" {
		t.Errorf("after the reset the capture shim runs %q, want the default", exe)
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
	if indexOf(argv, "--quiet") < 0 {
		t.Errorf("FolderExecCommand should be quiet: %v", argv)
	}
}

// ExecCommandQuiet's argv asks fleetd to leave the command out of its log
// (a poll loop's command would flood it); ExecCommand's does not.
func TestExecCommandQuietIsQuiet(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	if argv := b.ExecCommandQuiet("/ws", []string{"true"}).Args; indexOf(argv, "--quiet") < 0 || indexOf(argv, "--quiet") > indexOf(argv, "--") {
		t.Errorf("ExecCommandQuiet argv has no --quiet shim flag: %v", argv)
	}
	if argv := b.ExecCommand("/ws", []string{"true"}).Args; indexOf(argv, "--quiet") >= 0 {
		t.Errorf("ExecCommand argv is quiet: %v", argv)
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

// A create applies the connection's settings as they are now: the pool keeps
// a connection's client (and its copy of the record) across a change of the
// settings the client does not read, so the registry's record is the one
// read.
func TestCurrentRecordFollowsTheRegistry(t *testing.T) {
	newHandleRig(t)
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: ctx
  context: {cluster: c, user: u}
users:
- name: u
  user: {token: tok}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := fleet.KubeCluster{ID: handleCluster, Context: "ctx", KubeconfigPaths: []string{kubeconfig}, Namespace: handleNS}
	set := func(mode fleet.KubeImageMode) {
		t.Helper()
		r := rec
		r.Image.Mode = mode
		if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
			k.Clusters = []fleet.KubeCluster{r}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set("")
	p := NewPool(PoolOptions{Owner: func() (string, error) { return handleOwner, nil }})
	t.Cleanup(p.Close)
	p.Configure([]fleet.KubeCluster{rec})
	cl, err := p.Cluster(handleCluster)
	if err != nil {
		t.Fatal(err)
	}
	set(fleet.KubeImageGateway)
	gw := rec
	gw.Image.Mode = fleet.KubeImageGateway
	p.Configure([]fleet.KubeCluster{gw}) // the same connection: the client is kept
	if again, _ := p.Cluster(handleCluster); again != cl || again.Record.Image.Mode != "" {
		t.Fatalf("the pool reloaded the connection for a settings change (mode %q)", again.Record.Image.Mode)
	}
	if got := currentRecord(cl); got.Image.Mode != fleet.KubeImageGateway {
		t.Fatalf("currentRecord image mode %q, want the registry's %q", got.Image.Mode, fleet.KubeImageGateway)
	}
	// Without a readable registry, the pool's copy.
	if err := os.Remove(state.KubeClustersPath()); err != nil {
		t.Fatal(err)
	}
	if got := currentRecord(cl); got.ID != handleCluster {
		t.Fatalf("currentRecord without the registry = %+v", got)
	}
}

// The audio commands (for a consumer that runs them itself; fleetd opens
// them on the agent session) are the shim running fleet's own in the script
// class, quietly; the root class runs a privileged step as root.
func TestAudioAndRootCommandsBuildTheKubeExecArgv(t *testing.T) {
	r := newHandleRig(t)
	writeHandleCluster(t)
	inst := r.inst()
	inst.ContainerID = ContainerID(handleCluster, handleNS, handleSTS(), 1)
	b := newHandle(inst, r.pool)
	sink, ok := b.MicSinkCommand(inst.ContainerID)
	if !ok {
		t.Fatal("no mic sink command")
	}
	source, ok := b.OutputSourceCommand(inst.ContainerID)
	if !ok {
		t.Fatal("no output source command")
	}
	for name, tc := range map[string]struct {
		argv []string
		want []string
	}{
		"mic sink":      {sink.Args, fleetlaunch.MicSinkArgv()},
		"output source": {source.Args, fleetlaunch.OutputSourceArgv()},
	} {
		if flagValue(tc.argv, "--class") != "script" || indexOf(tc.argv, "--quiet") < 0 {
			t.Errorf("%s: not a quiet script-class shim: %v", name, tc.argv)
		}
		if dash := indexOf(tc.argv, "--"); dash < 0 || !slices.Equal(tc.argv[dash+1:], tc.want) {
			t.Errorf("%s: runs %v, want %v", name, tc.argv, tc.want)
		}
	}
	root := b.RootExecCommand("/ws", []string{"apt-get", "install"}).Args
	if flagValue(root, "--class") != "root" {
		t.Errorf("RootExecCommand: %v", root)
	}
}
