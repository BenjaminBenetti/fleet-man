package podinit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// The container cgroup's children.
const (
	// AgentCgroup holds PID 1, the pod agent and every exec the kubelet
	// starts (runtimes join an exec to PID 1's cgroup). It is never frozen.
	AgentCgroup = "fleet-agent"
	// UserCgroup holds the start script and everything users run: what a
	// clone's final delta freezes, and what an OOM kill picks from.
	UserCgroup = "fleet-user"
)

// LayoutFile is where pod-init records the layout for the pod agent, which
// moves the processes it spawns for users into the user cgroup.
const LayoutFile = podseed.RunDir + "/cgroup.json"

// cgroupMount is the cgroup v2 mount in the container.
var cgroupMount = "/sys/fs/cgroup"

// errNoLayout is a container whose cgroupfs pod-init cannot lay out:
// unprivileged (read-only), or cgroup v1.
var errNoLayout = errors.New("no cgroup layout")

// cgroupFS is the cgroupfs as the layout uses it: reads, whole writes (one
// write(2), as `echo > file`) and mkdir. Tests fake it.
type cgroupFS interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
	Mkdir(name string) error
}

type osCgroupFS struct{}

func (osCgroupFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (osCgroupFS) WriteFile(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (osCgroupFS) Mkdir(name string) error { return os.Mkdir(name, 0o755) }

// Layout is the cgroup layout pod-init made, as LayoutFile records it.
type Layout struct {
	V int `json:"v"`
	// Container, Agent and User are cgroupfs directories.
	Container string `json:"container"`
	Agent     string `json:"agent"`
	User      string `json:"user"`
	// Controllers are the ones enabled for the children.
	Controllers []string `json:"controllers,omitempty"`
	// OOMGroup is whether pod-init keeps the container's memory.oom.group
	// at 0 (one process dies on an OOM, not the container).
	OOMGroup bool `json:"oom_group"`
	// UserMemoryMax is the user cgroup's memory.max, -1 with no limit.
	UserMemoryMax int64 `json:"user_memory_max"`

	fs           cgroupFS
	margin       int64
	containerMax int64
}

// containerCgroup is the container's cgroup path from /proc/self/cgroup: the
// unified ("0::") line. Under a host cgroup namespace (containerd, CRI-O with
// a privileged pod) it is the full path; under a private one (cri-dockerd) it
// is "/", the namespace root.
func containerCgroup(procSelfCgroup []byte) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(procSelfCgroup))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return path.Clean("/" + rest), true
		}
	}
	return "", false
}

// layoutControllers are the controllers the children get, when the container
// cgroup has them.
var layoutControllers = []string{"cpu", "memory", "pids"}

// moveRetries bounds the move-then-enable loop: a process forked between the
// move and the write makes the kernel refuse (EBUSY), so it goes again.
const moveRetries = 50

// setupCgroups lays out the container's cgroup (dir, a cgroupfs path): the
// two children, every process moved into the agent one, the controllers
// enabled for them, memory.oom.group 0 and the user cgroup's memory.max. A
// read-only cgroupfs is errNoLayout.
func setupCgroups(cfs cgroupFS, dir string, margin int64) (*Layout, error) {
	controllers, err := cfs.ReadFile(path.Join(dir, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a cgroup v2 dir: %v", errNoLayout, dir, err)
	}
	l := &Layout{V: 1, Container: dir, Agent: path.Join(dir, AgentCgroup), User: path.Join(dir, UserCgroup),
		UserMemoryMax: -1, fs: cfs, margin: margin, containerMax: -1}
	for _, child := range []string{l.Agent, l.User} {
		if err := cfs.Mkdir(child); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
				return nil, fmt.Errorf("%w: the cgroupfs is read-only (an unprivileged pod)", errNoLayout)
			}
			return nil, fmt.Errorf("create %s: %w", child, err)
		}
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(controllers)) {
		have[c] = true
	}
	var enable []string
	for _, c := range layoutControllers {
		if have[c] {
			enable = append(enable, c)
			l.Controllers = append(l.Controllers, c)
		}
	}
	if err := l.moveAndEnable(enable); err != nil {
		return nil, err
	}
	if err := cfs.WriteFile(path.Join(dir, "memory.oom.group"), []byte("0")); err == nil {
		l.OOMGroup = true
	}
	l.syncMemoryMax()
	return l, nil
}

// moveAndEnable moves every process of the container cgroup into the agent
// cgroup, then enables the controllers for the children: a cgroup with
// controllers enabled below it may hold no process of its own.
func (l *Layout) moveAndEnable(enable []string) error {
	var plus []string
	for _, c := range enable {
		plus = append(plus, "+"+c)
	}
	var lastErr error
	for range moveRetries {
		procs, err := l.fs.ReadFile(path.Join(l.Container, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("read the container's processes: %w", err)
		}
		for _, pid := range strings.Fields(string(procs)) {
			if err := l.fs.WriteFile(path.Join(l.Agent, "cgroup.procs"), []byte(pid)); err != nil && !errors.Is(err, syscall.ESRCH) {
				lastErr = fmt.Errorf("move process %s into %s: %w", pid, AgentCgroup, err)
			}
		}
		if len(plus) == 0 {
			return nil
		}
		err = l.fs.WriteFile(path.Join(l.Container, "cgroup.subtree_control"), []byte(strings.Join(plus, " ")))
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("enable %s for the children: %w", strings.Join(enable, ", "), err)
		}
		lastErr = err
	}
	return fmt.Errorf("enable the children's controllers: processes kept arriving: %w", lastErr)
}

// memoryMargin is how far below a limit the user cgroup's memory.max goes:
// enough for the agent, PID 1 and an exec or two; a thirty-second of the
// limit within [64 MiB, 256 MiB], never more than a quarter of it.
func memoryMargin(limit, margin int64) int64 {
	if margin <= 0 {
		margin = min(max(limit/32, 64<<20), 256<<20)
	}
	return min(margin, limit/4)
}

// syncMemoryMax sets the user cgroup's memory.max just under the container's
// memory.max whenever that changes (a resize), and reports whether it wrote
// one. With no limit it leaves the user cgroup unlimited.
func (l *Layout) syncMemoryMax() bool {
	data, err := l.fs.ReadFile(path.Join(l.Container, "memory.max"))
	if err != nil {
		return false
	}
	limit := int64(-1)
	if s := strings.TrimSpace(string(data)); s != "max" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		limit = n
	}
	if limit == l.containerMax {
		return false
	}
	l.containerMax = limit
	value, userMax := "max", int64(-1)
	if limit > 0 {
		userMax = limit - memoryMargin(limit, l.margin)
		value = strconv.FormatInt(userMax, 10)
	}
	if err := l.fs.WriteFile(path.Join(l.User, "memory.max"), []byte(value)); err != nil {
		return false
	}
	l.UserMemoryMax = userMax
	return true
}

// poll re-applies what the kubelet may undo: on containerd every container
// update (an in-place resize, a static CPU manager reconcile) writes
// memory.oom.group back to 1. It returns notes on what it fixed.
func (l *Layout) poll() []string {
	var notes []string
	if l.OOMGroup {
		file := path.Join(l.Container, "memory.oom.group")
		if data, err := l.fs.ReadFile(file); err == nil && strings.TrimSpace(string(data)) == "1" {
			if err := l.fs.WriteFile(file, []byte("0")); err == nil {
				notes = append(notes, "memory.oom.group was reset to 1; set it back to 0")
			}
		}
	}
	if l.syncMemoryMax() {
		notes = append(notes, fmt.Sprintf("memory limit is now %d; %s memory.max %d", l.containerMax, UserCgroup, l.UserMemoryMax))
	}
	return notes
}

// write records the layout at file for the pod agent.
func (l *Layout) write(file string) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// ReadLayout reads the layout pod-init recorded; the pod agent uses it to put
// user processes into the user cgroup. fs.ErrNotExist means none was made.
func ReadLayout(file string) (Layout, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Layout{}, err
	}
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		return Layout{}, fmt.Errorf("parse %s: %w", file, err)
	}
	return l, nil
}
