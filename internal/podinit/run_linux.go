//go:build linux

package podinit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"golang.org/x/sys/unix"
)

// PodUIDEnv carries the pod's uid to pod-init (the renderer sets it from the
// downward API); it marks runs in the rolling file and never reaches the start
// script.
const PodUIDEnv = "FLEET_POD_UID"

// Paths pod-init works on; tests point them elsewhere.
var (
	selfExe        = "/proc/self/exe"
	procSelfCgroup = "/proc/self/cgroup"
	procSelfOOM    = "/proc/self/oom_score_adj"
	// etcRoot holds the etc/passwd and etc/group execas resolves the
	// start script's user from.
	etcRoot    = "/"
	etcPasswd  = "/etc/passwd"
	etcGroup   = "/etc/group"
	binDir     = podseed.BinDir
	fleetLink  = podseed.FleetLink
	layoutFile = LayoutFile
	freezeFile = FreezeMarkerFile
	// pollInterval is how often the cgroup layout is re-checked.
	pollInterval = 5 * time.Second
	// freezeWatchInterval is how often the freeze watchdog looks.
	freezeWatchInterval = time.Second
	// drainTimeout bounds how long PID 1 keeps copying output after the
	// start script exits: a daemon it started can hold the pipe forever.
	drainTimeout = 2 * time.Second
)

// Run is `fleet pod-init --spec specPath`, PID 1 of an instance pod. It
// returns the exit code the container ends with (it re-execs another binary
// instead when the spec names one; see chooseBinary).
func Run(specPath string, stdout, stderr *os.File) int {
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, "fleet pod-init: "+format+"\n", args...)
	}
	// A write to a closed container log must not kill PID 1.
	pipes := make(chan os.Signal, 1)
	signal.Notify(pipes, unix.SIGPIPE)
	defer signal.Stop(pipes)
	// Signals that arrive before the start script exists (a pod deleted
	// while it boots) wait here and reach it once it starts.
	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, forwardedSignals()...)
	defer signal.Stop(sigs)

	spec, err := LoadSpec(specPath)
	if err != nil {
		logf("%v", err)
		return execas.ExitFailed
	}
	self, err := podseed.FileSHA256(selfExe)
	if err != nil {
		logf("hash this binary: %v", err)
	} else if choice := chooseBinary(spec, self, binDir); choice.exec != "" {
		logf("this binary is not the spec's; running %s", filepath.Base(choice.exec))
		env := append(os.Environ(), reexecEnv+"=1")
		err := syscall.Exec(choice.exec, append([]string{choice.exec}, os.Args[1:]...), env)
		logf("exec %s: %v; running on", choice.exec, err)
	} else if choice.warning != "" {
		logf("%s", choice.warning)
	}
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)

	runID := randomID()
	logDir := spec.LogDir
	if logDir == "" {
		logDir = podseed.LogDir
	}
	rolling, err := OpenRolling(logDir, spec.LogMax, RunMark{ID: runID, PodUID: os.Getenv(PodUIDEnv), Start: time.Now().UTC()})
	if err != nil {
		logf("no rolling copy of the output: %v", err)
	}
	if rolling != nil {
		defer rolling.Close()
	}
	out := teeWriter{w: stdout, log: rolling}
	errOut := teeWriter{w: stderr, log: rolling}
	warnf := func(format string, args ...any) {
		fmt.Fprintf(errOut, "fleet pod-init: "+format+"\n", args...)
	}

	prepare(spec, warnf)
	var layout *Layout
	if spec.Cgroups != CgroupsOff {
		layout = layoutCgroups(spec, warnf)
	}
	// Last, so pod-init's own files and cgroups keep their modes; the start
	// script inherits it.
	if mask, err := spec.umask(); spec.Umask != "" && err == nil {
		unix.Umask(mask)
	}

	plan, err := planStart(spec)
	if err != nil {
		warnf("start %s: %v", spec.Argv[0], err)
		return execas.ExitCode(err)
	}
	return supervise(spec, plan, layout, sigs, out, errOut, warnf)
}

// prepare does what docker's runtime does before the container's process:
// the fleet link, a passwd entry for an unknown uid, the host and domain
// names, the DinD MTU, the rlimits and the OOM score (set on PID 1, so the
// start script and the agent's execs inherit it). Each is best-effort with a
// warning: the start script runs regardless, as docker's would.
func prepare(spec Spec, warnf func(string, ...any)) {
	if _, err := os.Lstat(fleetLink); errors.Is(err, fs.ErrNotExist) {
		if fi, err := os.Stat(path.Dir(fleetLink)); err == nil && fi.IsDir() {
			if err := os.Symlink(podseed.CurrentBinary, fleetLink); err != nil {
				warnf("restore %s: %v", fleetLink, err)
			}
		}
	}
	if uid := os.Getuid(); uid != 0 {
		if _, err := ensurePodUser(etcPasswd, etcGroup, uid, os.Getgid(), spec.RemoteUser); err != nil {
			warnf("%v", err)
		}
	}
	if spec.Hostname != "" {
		if cur, _ := os.Hostname(); cur != spec.Hostname {
			if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
				warnf("set the hostname %s: %v (the pod keeps %s)", spec.Hostname, err, cur)
			}
		}
	}
	if spec.Domainname != "" {
		var uts unix.Utsname
		cur := ""
		if unix.Uname(&uts) == nil {
			cur = unix.ByteSliceToString(uts.Domainname[:])
		}
		if cur != spec.Domainname {
			if err := unix.Setdomainname([]byte(spec.Domainname)); err != nil {
				warnf("set the domain name %s: %v (the pod keeps %s)", spec.Domainname, err, cur)
			}
		}
	}
	if m := spec.DockerMTU; m != nil {
		mtu, err := m.MTU, error(nil)
		if mtu == 0 {
			mtu, err = podMTU()
		}
		file := m.DaemonJSON
		if file == "" {
			file = DefaultDockerDaemonJSON
		}
		if err == nil {
			_, err = mergeDockerMTU(file, mtu)
		}
		if err != nil {
			warnf("docker mtu: %v", err)
		}
	}
	for _, w := range applyRlimits(spec.Rlimits) {
		warnf("%s", w)
	}
	if adj := spec.OOMScoreAdj; adj != nil {
		if err := os.WriteFile(procSelfOOM, []byte(strconv.Itoa(*adj)), 0); err != nil {
			warnf("set the OOM score adjustment %d: %v", *adj, err)
		}
	}
}

// layoutCgroups makes the cgroup layout where the cgroupfs allows it and keeps
// it right; nil when it does not.
func layoutCgroups(spec Spec, warnf func(string, ...any)) *Layout {
	data, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		warnf("cgroup layout: %v", err)
		return nil
	}
	cg, ok := containerCgroup(data)
	if !ok {
		return nil // cgroup v1: nothing to do
	}
	l, err := setupCgroups(osCgroupFS{}, path.Join(cgroupMount, cg), spec.MemoryMargin)
	if errors.Is(err, errNoLayout) {
		return nil
	}
	if err != nil {
		warnf("cgroup layout: %v", err)
		return nil
	}
	if err := l.write(layoutFile); err != nil {
		warnf("record the cgroup layout: %v", err)
	}
	go func() {
		for range time.Tick(pollInterval) {
			for _, note := range l.poll() {
				warnf("%s", note)
			}
		}
	}()
	// The freeze watchdog: PID 1 outlives the agent that froze the user
	// cgroup (and fleetd, and their session), so a freeze nobody thaws ends
	// here, shortly after its deadline.
	go func() {
		for now := range time.Tick(freezeWatchInterval) {
			if note := l.WatchFreeze(freezeFile, now); note != "" {
				warnf("%s", note)
			}
		}
	}()
	return l
}

// planStart resolves the start script as containerUser would run it: docker's
// -u, -w and environment rules, through execas's explicit class.
func planStart(spec Spec) (*execas.Plan, error) {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); name == PodUIDEnv || name == reexecEnv {
			continue
		}
		env = append(env, kv)
	}
	for name, value := range spec.Env {
		env = append(env, name+"="+value)
	}
	host := &execas.Host{
		Spec:       execas.Spec{ContainerUser: spec.User, WorkingDir: spec.WorkingDir},
		Root:       etcRoot,
		ProcessEnv: env,
		Self:       execas.CurrentCreds(),
		SingleUID:  !execas.CanSwitchUser(),
	}
	return host.Plan(context.Background(), execas.Request{Class: execas.ClassExplicit, Argv: spec.Argv})
}

// supervise starts the start script and is its PID 1 until it exits.
func supervise(spec Spec, plan *execas.Plan, layout *Layout, sigs <-chan os.Signal, out, errOut io.Writer, warnf func(string, ...any)) int {
	stopSignal := unix.SIGTERM
	if spec.StopSignal != "" {
		if n, err := SignalNumber(spec.StopSignal); err == nil {
			stopSignal = syscall.Signal(n)
		}
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		warnf("%v", err)
		return execas.ExitFailed
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		warnf("%v", err)
		return execas.ExitFailed
	}
	pid, err := spawn(plan, layout, outW, errW)
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		warnf("start %s: %v", plan.Argv[0], err)
		return execas.ExitCode(err)
	}
	var copying sync.WaitGroup
	for _, c := range []struct {
		r *os.File
		w io.Writer
	}{{outR, out}, {errR, errOut}} {
		copying.Add(1)
		go func() {
			defer copying.Done()
			_, _ = io.Copy(c.w, c.r)
		}()
	}

	exited := make(chan unix.WaitStatus, 1)
	go reap(pid, exited)
	for {
		select {
		case sig := <-sigs:
			s := sig.(syscall.Signal)
			if s == unix.SIGTERM {
				s = stopSignal
			}
			forward(pid, s, spec.Init)
		case ws := <-exited:
			done := make(chan struct{})
			go func() {
				copying.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(drainTimeout):
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
}

// forward passes sig to the start script. With --init it always does, as tini;
// without, only when the script catches it: the kernel drops an uncaught signal
// to a PID 1, and docker's PID 1 is the script itself.
func forward(pid int, sig syscall.Signal, init bool) {
	if !init {
		mask, err := caughtSignals(pid)
		if err == nil && !catches(mask, sig) {
			return
		}
	}
	_ = unix.Kill(pid, sig)
}

// reap waits for every child PID 1 inherits, reporting the start script's
// status on exited once it ends.
func reap(main int, exited chan<- unix.WaitStatus) {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			// No children at all: the script is gone without a status.
			exited <- ws
			return
		}
		if pid == main && (ws.Exited() || ws.Signaled()) {
			exited <- ws
			return
		}
	}
}

// spawn starts plan's command in the user cgroup (CLONE_INTO_CGROUP, else
// moved right after the start), in a session of its own, with stdin on
// /dev/null and stdout and stderr on the pipes.
func spawn(plan *execas.Plan, layout *Layout, stdout, stderr *os.File) (int, error) {
	if fi, err := os.Stat(plan.Dir); err != nil || !fi.IsDir() {
		return 0, &execas.ExitError{Code: execas.ExitCannotRun, Err: fmt.Errorf("chdir %s: %w", plan.Dir, errOrNotDir(err))}
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()
	attr := &syscall.SysProcAttr{Setsid: true}
	if plan.Switch {
		attr.Credential = &syscall.Credential{Uid: plan.Identity.UID, Gid: plan.Identity.GID, Groups: plan.Identity.Groups}
	}
	procAttr := &syscall.ProcAttr{Dir: plan.Dir, Env: plan.Env, Files: []uintptr{devnull.Fd(), stdout.Fd(), stderr.Fd()}, Sys: attr}

	var cgroup *os.File
	if layout != nil {
		if cgroup, err = os.Open(layout.User); err == nil {
			defer cgroup.Close()
			attr.UseCgroupFD, attr.CgroupFD = true, int(cgroup.Fd())
		}
	}
	pid, err := syscall.ForkExec(plan.Path, plan.Argv, procAttr)
	if err != nil && attr.UseCgroupFD && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.EACCES) {
		// A kernel without clone3's CLONE_INTO_CGROUP (before 5.7).
		attr.UseCgroupFD = false
		pid, err = syscall.ForkExec(plan.Path, plan.Argv, procAttr)
		if err == nil {
			_ = osCgroupFS{}.WriteFile(path.Join(layout.User, "cgroup.procs"), []byte(fmt.Sprint(pid)))
		}
	}
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return 0, &execas.ExitError{Code: execas.ExitNotFound, Err: fmt.Errorf("exec %s: %w", plan.Argv[0], err)}
		}
		return 0, &execas.ExitError{Code: execas.ExitCannotRun, Err: fmt.Errorf("exec %s: %w", plan.Argv[0], err)}
	}
	return pid, nil
}

func errOrNotDir(err error) error {
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return pe.Err
		}
		return err
	}
	return syscall.ENOTDIR
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
