//go:build linux

package podinit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// helperEnv makes the test binary pod-init (see TestMain); the PODINIT_TEST_*
// variables point it at the test's fake pod.
const helperEnv = "PODINIT_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "pod-init" {
		for env, v := range map[string]*string{
			"PODINIT_TEST_BIN_DIR": &binDir, "PODINIT_TEST_FLEET_LINK": &fleetLink,
			"PODINIT_TEST_LAYOUT": &layoutFile, "PODINIT_TEST_CGROUP": &procSelfCgroup,
			"PODINIT_TEST_CGROUP_MOUNT": &cgroupMount, "PODINIT_TEST_ROUTE": &procNetRoute,
			"PODINIT_TEST_SYSNET": &sysClassNet,
		} {
			if s := os.Getenv(env); s != "" {
				*v = s
			}
		}
		if root := os.Getenv("PODINIT_TEST_ETC_ROOT"); root != "" {
			etcRoot, etcPasswd, etcGroup = root, filepath.Join(root, "etc/passwd"), filepath.Join(root, "etc/group")
		}
		os.Exit(Run(os.Getenv("PODINIT_TEST_SPEC"), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	if os.Getenv("FLEET_PODSEED_REQUIRE_ROOT") == "1" {
		t.Fatal("FLEET_PODSEED_REQUIRE_ROOT is set but this is not root")
	}
	t.Skip("needs root")
}

// syncBuffer is a bytes.Buffer safe to read while a process writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakePod is the filesystem a test pod-init runs against.
type fakePod struct {
	dir, binDir, link, etc, logDir, layout string
}

func newFakePod(t *testing.T) *fakePod {
	t.Helper()
	dir := t.TempDir()
	p := &fakePod{
		dir: dir, binDir: filepath.Join(dir, "opt/fleet/bin"), link: filepath.Join(dir, "usr/bin/fleet"),
		etc: filepath.Join(dir, "root"), logDir: filepath.Join(dir, "opt/fleet/log"), layout: filepath.Join(dir, "run/cgroup.json"),
	}
	for _, d := range []string{p.binDir, filepath.Dir(p.link), filepath.Join(p.etc, "etc"), filepath.Join(dir, "home/vscode")} {
		mustMkdirAll(t, d)
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\nvscode:x:4242:4242::" + filepath.Join(dir, "home/vscode") + ":/bin/sh\n"
	if err := os.WriteFile(filepath.Join(p.etc, "etc/passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.etc, "etc/group"), []byte("root:x:0:\nvscode:x:4242:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type podInit struct {
	cmd            *exec.Cmd
	stdout, stderr *syncBuffer
	done           chan int
}

// start runs pod-init on spec in the fake pod.
func (p *fakePod) start(t *testing.T, spec Spec, env ...string) *podInit {
	t.Helper()
	spec.V = SpecVersion
	if spec.Cgroups == "" {
		spec.Cgroups = CgroupsOff
	}
	if spec.LogDir == "" {
		spec.LogDir = p.logDir
	}
	data, _ := json.Marshal(spec)
	specFile := filepath.Join(p.dir, "pid1.json")
	if err := os.WriteFile(specFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=pod-init", "PODINIT_TEST_SPEC="+specFile,
		"PODINIT_TEST_BIN_DIR="+p.binDir, "PODINIT_TEST_FLEET_LINK="+p.link,
		"PODINIT_TEST_ETC_ROOT="+p.etc, "PODINIT_TEST_LAYOUT="+p.layout, reexecEnv+"=")
	cmd.Env = append(cmd.Env, env...)
	pi := &podInit{cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1)}
	cmd.Stdout, cmd.Stderr = pi.stdout, pi.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			pi.done <- 0
		case errors.As(err, &exitErr):
			pi.done <- exitErr.ExitCode()
		default:
			pi.done <- -1
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return pi
}

func (pi *podInit) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-pi.done:
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("pod-init did not exit\nstdout: %s\nstderr: %s", pi.stdout, pi.stderr)
	}
	return 0
}

// waitFor waits for want in pod-init's stdout.
func (pi *podInit) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(pi.stdout.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the output\nstdout: %s\nstderr: %s", want, pi.stdout, pi.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (pi *podInit) exited() bool {
	select {
	case code := <-pi.done:
		pi.done <- code
		return true
	default:
		return false
	}
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestPodInitRunsTheStartScriptAndCopiesItsOutput(t *testing.T) {
	p := newFakePod(t)
	pi := p.start(t, Spec{Argv: sh("echo out; echo err >&2; exit 7")}, PodUIDEnv+"=pod-uid-1")
	if code := pi.wait(t); code != 7 {
		t.Fatalf("exit %d, want the script's 7\nstderr: %s", code, pi.stderr)
	}
	if !strings.Contains(pi.stdout.String(), "out\n") || !strings.Contains(pi.stderr.String(), "err\n") {
		t.Fatalf("stdout %q stderr %q", pi.stdout, pi.stderr)
	}
	runs, err := ReadRolling(p.logDir)
	if err != nil || len(runs) != 1 {
		t.Fatalf("rolling: %+v %v", runs, err)
	}
	if !strings.Contains(string(runs[0].Data), "out\n") || !strings.Contains(string(runs[0].Data), "err\n") || runs[0].PodUID != "pod-uid-1" {
		t.Fatalf("rolling run %+v (%q)", runs[0].RunMark, runs[0].Data)
	}
	// A second start appends a run.
	pi = p.start(t, Spec{Argv: sh("echo second")})
	pi.wait(t)
	if runs, _ := ReadRolling(p.logDir); len(runs) != 2 || string(runs[1].Data) != "second\n" {
		t.Fatalf("runs %+v", runs)
	}
}

func TestPodInitTranslatesTheStopSignal(t *testing.T) {
	p := newFakePod(t)
	pi := p.start(t, Spec{StopSignal: "SIGINT",
		Argv: sh(`trap 'echo got-int; exit 3' INT; trap 'echo got-term; exit 4' TERM; echo ready; while :; do sleep 0.05; done`)})
	pi.waitFor(t, "ready")
	_ = pi.cmd.Process.Signal(syscall.SIGTERM)
	if code := pi.wait(t); code != 3 || !strings.Contains(pi.stdout.String(), "got-int") {
		t.Fatalf("exit %d, output %q: the kubelet's SIGTERM should arrive as SIGINT", code, pi.stdout)
	}
}

func TestPodInitForwardsOnlyCaughtSignalsWithoutInit(t *testing.T) {
	p := newFakePod(t)
	pi := p.start(t, Spec{Argv: sh(`echo "ready $$"; exec sleep 30`)})
	pi.waitFor(t, "ready")
	pid, _ := strconv.Atoi(strings.Fields(strings.TrimSpace(pi.stdout.String()))[1])
	_ = pi.cmd.Process.Signal(syscall.SIGTERM)
	time.Sleep(700 * time.Millisecond)
	if pi.exited() {
		t.Fatal("an uncaught SIGTERM reached the start script; a PID 1 would have dropped it")
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if code := pi.wait(t); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit %d, want 137", code)
	}

	// With --init, tini forwards it and sleep dies of it.
	pi = p.start(t, Spec{Init: true, Argv: sh(`echo ready; exec sleep 30`)})
	pi.waitFor(t, "ready")
	_ = pi.cmd.Process.Signal(syscall.SIGTERM)
	if code := pi.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %d, want 143", code)
	}
}

func TestPodInitReapsOrphans(t *testing.T) {
	p := newFakePod(t)
	pi := p.start(t, Spec{Argv: sh(`(sleep 0.2 &); (sleep 0.3 &); echo ready; sleep 1.5; echo done`)})
	pi.waitFor(t, "ready")
	time.Sleep(900 * time.Millisecond)
	if z := zombiesOf(pi.cmd.Process.Pid); len(z) != 0 {
		t.Fatalf("pod-init left zombies %v", z)
	}
	if code := pi.wait(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// zombiesOf lists the zombie children of pid.
func zombiesOf(pid int) []int {
	var z []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...
		rest := string(data[bytes.LastIndexByte(data, ')')+2:])
		f := strings.Fields(rest)
		if len(f) > 1 && f[0] == "Z" && f[1] == strconv.Itoa(pid) {
			z = append(z, child)
		}
	}
	return z
}

func TestPodInitAppliesRlimits(t *testing.T) {
	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &cur); err != nil {
		t.Fatal(err)
	}
	p := newFakePod(t)
	pi := p.start(t, Spec{Rlimits: []Rlimit{{Name: "nofile", Soft: 321, Hard: int64(cur.Max)}}, Argv: sh("ulimit -n")})
	if code := pi.wait(t); code != 0 || strings.TrimSpace(pi.stdout.String()) != "321" {
		t.Fatalf("exit %d, ulimit -n %q\n%s", code, pi.stdout, pi.stderr)
	}
	if os.Geteuid() == 0 {
		return // root may raise the hard limit
	}
	pi = p.start(t, Spec{Rlimits: []Rlimit{{Name: "nofile", Soft: int64(cur.Max) + 10, Hard: int64(cur.Max) + 10}}, Argv: sh("ulimit -n")})
	if code := pi.wait(t); code != 0 || !strings.Contains(pi.stderr.String(), "CAP_SYS_RESOURCE") {
		t.Fatalf("exit %d, stderr %q: a hard raise without the capability should warn and run on", code, pi.stderr)
	}
	if got := strings.TrimSpace(pi.stdout.String()); got != strconv.FormatUint(cur.Max, 10) {
		t.Fatalf("soft limit %q, want it clamped to the hard %d", got, cur.Max)
	}
}

func TestPodInitPreparesThePod(t *testing.T) {
	p := newFakePod(t)
	dir := t.TempDir()
	route := filepath.Join(dir, "route")
	_ = os.WriteFile(route, []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nnet0\t00000000\t0100F40A\t0003\t0\t0\t0\t00000000\n"), 0o644)
	mustMkdirAll(t, filepath.Join(dir, "net/net0"))
	_ = os.WriteFile(filepath.Join(dir, "net/net0/mtu"), []byte("1430\n"), 0o644)
	daemonJSON := filepath.Join(dir, "etc/docker/daemon.json")
	pi := p.start(t, Spec{RemoteUser: "vscode", DockerMTU: &DockerMTU{DaemonJSON: daemonJSON}, Argv: sh("readlink " + p.link)},
		"PODINIT_TEST_ROUTE="+route, "PODINIT_TEST_SYSNET="+filepath.Join(dir, "net"))
	if code := pi.wait(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, pi.stderr)
	}
	if strings.TrimSpace(pi.stdout.String()) != podseed.CurrentBinary {
		t.Fatalf("/usr/bin/fleet -> %q, want it restored", pi.stdout)
	}
	if data, _ := os.ReadFile(daemonJSON); !strings.Contains(string(data), `"mtu": 1430`) {
		t.Fatalf("daemon.json %s", data)
	}
	passwd, _ := os.ReadFile(filepath.Join(p.etc, "etc/passwd"))
	if os.Geteuid() != 0 {
		want := fmt.Sprintf("fleet:x:%d:%d:fleet pod user:%s:/bin/sh\n", os.Getuid(), os.Getgid(), filepath.Join(p.dir, "home/vscode"))
		if !strings.HasSuffix(string(passwd), want) {
			t.Fatalf("passwd:\n%s\nwant it to end in %q", passwd, want)
		}
	}
	// An existing link, or whatever the user put there, is left alone.
	_ = os.Remove(p.link)
	_ = os.WriteFile(p.link, []byte("the user's own"), 0o755)
	pi = p.start(t, Spec{Argv: sh("true")})
	pi.wait(t)
	if data, _ := os.ReadFile(p.link); string(data) != "the user's own" {
		t.Fatal("pod-init replaced an existing /usr/bin/fleet")
	}
}

func TestPodInitReexecsTheSpecsBinary(t *testing.T) {
	p := newFakePod(t)
	script := []byte("#!/bin/sh\necho \"spec binary ran: $FLEET_POD_INIT_REEXEC $*\"\nexit 5\n")
	sum := sha256.Sum256(script)
	sha := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(p.binDir, podseed.BinaryName(sha)), script, 0o755); err != nil {
		t.Fatal(err)
	}
	pi := p.start(t, Spec{BinarySHA256: sha, Argv: sh("echo the wrong binary ran the script")})
	if code := pi.wait(t); code != 5 || !strings.Contains(pi.stdout.String(), "spec binary ran: 1 -test.run=^$") {
		t.Fatalf("exit %d, stdout %q\nstderr %s", code, pi.stdout, pi.stderr)
	}
	// Nothing better on disk: run on, with a warning.
	pi = p.start(t, Spec{BinarySHA256: strings.Repeat("a", 64), Argv: sh("echo ran on")})
	if code := pi.wait(t); code != 0 || !strings.Contains(pi.stdout.String(), "ran on") || !strings.Contains(pi.stderr.String(), "running on") {
		t.Fatalf("exit %d, stdout %q stderr %q", code, pi.stdout, pi.stderr)
	}
}

func TestPodInitStartFailuresUseDockersCodes(t *testing.T) {
	p := newFakePod(t)
	// Without root (capability tier C) everything runs as the pod's uid, so
	// a user the image lacks is no error.
	unknownUser := 0
	if os.Geteuid() == 0 {
		unknownUser = 126
	}
	for name, tc := range map[string]struct {
		spec Spec
		code int
	}{
		"missing executable": {Spec{Argv: []string{"/no/such/start"}}, 127},
		"missing workdir":    {Spec{Argv: sh("true"), WorkingDir: "/no/such/dir"}, 126},
		"unknown user":       {Spec{Argv: sh("true"), User: "nosuchuser"}, unknownUser},
	} {
		t.Run(name, func(t *testing.T) {
			pi := p.start(t, tc.spec)
			if code := pi.wait(t); code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, pi.stderr)
			}
		})
	}
	if code := p.start(t, Spec{}).wait(t); code != 125 {
		t.Fatalf("a bad spec: exit %d, want 125", code)
	}
}

func TestPodInitAsRootDropsToContainerUser(t *testing.T) {
	requireRoot(t)
	p := newFakePod(t)
	home := filepath.Join(p.dir, "home/vscode")
	pi := p.start(t, Spec{User: "vscode", WorkingDir: "/", Argv: sh(`echo "$(id -u):$(id -g) $HOME $(pwd)"`)}, "HOME=/root")
	if code := pi.wait(t); code != 0 {
		t.Fatalf("exit %d\n%s", code, pi.stderr)
	}
	if got := strings.TrimSpace(pi.stdout.String()); got != "4242:4242 "+home+" /" {
		t.Fatalf("got %q", got)
	}
}

func TestSigCgt(t *testing.T) {
	mask, err := parseSigCgt("Name:\tsh\nSigIgn:\t0000000000000004\nSigCgt:\t0000000000004002\n")
	if err != nil {
		t.Fatal(err)
	}
	if !catches(mask, syscall.SIGINT) || !catches(mask, syscall.SIGTERM) || catches(mask, syscall.SIGHUP) || catches(mask, 0) {
		t.Fatalf("mask %x", mask)
	}
	if _, err := parseSigCgt("Name:\tx\n"); err == nil {
		t.Fatal("no SigCgt parsed")
	}
	if mask, err := caughtSignals(os.Getpid()); err != nil || mask == 0 {
		t.Fatalf("this test process catches nothing? %x %v", mask, err)
	}
}
