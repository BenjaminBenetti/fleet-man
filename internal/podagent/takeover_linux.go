//go:build linux

package podagent

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// One main agent runs per pod: it holds an exclusive flock on the pid file
// (ControlDir/.pod-agent.pid), which records its pid and a random nonce, and
// listens on RunDir/agent/pod-agent-<nonce>.sock. A newly accepted agent asks
// the recorded one, by nonce, to release; one that does not answer in time
// (hung, stopped) is sent SIGTERM, then SIGKILL, but only once /proc/locks
// shows that very pid holding the lock, so a reused pid is never signalled.
// Held commands need no handing over: their shepherds outlive both agents.

// lockPoll is how often a waiting agent retries the lock.
const lockPoll = 20 * time.Millisecond

// pidRecord is the pid file's content.
type pidRecord struct {
	PID   int    `json:"pid"`
	Nonce string `json:"nonce"`
}

type releaseRequest struct {
	Op    string `json:"op"`
	Nonce string `json:"nonce"`
}

type releaseReply struct {
	OK  bool `json:"ok"`
	PID int  `json:"pid"`
}

// pidLock is the main agent's hold on the pid file and its takeover socket.
type pidLock struct {
	f     *os.File
	ln    *net.UnixListener
	nonce string
	once  sync.Once
}

// release gives the lock up and stops listening. Idempotent.
func (l *pidLock) release() {
	l.once.Do(func() {
		_ = l.ln.Close()
		_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
		_ = l.f.Close()
	})
}

// takeOver makes this agent the pod's main agent.
func (a *agent) takeOver() (*pidLock, error) {
	o := a.opts
	if err := os.MkdirAll(o.ControlDir, 0o755); err != nil {
		return nil, err
	}
	agentDir, err := privateDir(o.RunDir, agentDirName)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(o.ControlDir, pidFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := a.acquire(f, agentDir); err != nil {
		_ = f.Close()
		return nil, err
	}
	nonce := newResumeID()
	sock := filepath.Join(agentDir, "pod-agent-"+nonce+".sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	ln.SetUnlinkOnClose(true)
	_ = os.Chmod(sock, 0o600)
	rec, _ := json.Marshal(pidRecord{PID: os.Getpid(), Nonce: nonce})
	if err := f.Truncate(0); err == nil {
		_, err = f.WriteAt(append(rec, '\n'), 0)
		if err == nil {
			err = f.Sync()
		}
	}
	if err != nil {
		_ = ln.Close()
		_ = f.Close()
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	l := &pidLock{f: f, ln: ln, nonce: nonce}
	go a.serveRelease(l)
	return l, nil
}

// acquire takes the pid file's lock, taking it from its holder if need be.
func (a *agent) acquire(f *os.File, agentDir string) error {
	if tryLock(f) {
		return nil
	}
	rec := readRecord(f)
	holder := lockHolder(f)
	if holder == os.Getpid() {
		return fmt.Errorf("this process already holds %s", f.Name())
	}
	a.log.Info("taking over from the running pod agent", "pid", strconv.Itoa(holder))
	if holder > 0 && rec.PID == holder && podwire.ValidResumeID(rec.Nonce) &&
		askRelease(filepath.Join(agentDir, "pod-agent-"+rec.Nonce+".sock"), rec.Nonce, a.opts.TakeoverAsk) &&
		waitLock(f, a.opts.TakeoverKill) {
		return nil
	}
	if tryLock(f) {
		return nil
	}
	// It did not release: it is hung, or not listening. Signal the process
	// that holds the lock now, and only if it is a pod agent of this uid.
	holder = lockHolder(f)
	if holder > 0 && holder != os.Getpid() && isPodAgent(holder) {
		a.log.Warn("the running pod agent did not release; stopping it", "pid", strconv.Itoa(holder))
		_ = syscall.Kill(holder, syscall.SIGTERM)
		if waitLock(f, a.opts.TakeoverKill) {
			return nil
		}
		_ = syscall.Kill(holder, syscall.SIGKILL)
	}
	if waitLock(f, a.opts.TakeoverKill) {
		return nil
	}
	return fmt.Errorf("%s is still locked by pid %d", f.Name(), lockHolder(f))
}

func tryLock(f *os.File) bool {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
}

func waitLock(f *os.File, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if tryLock(f) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}

func readRecord(f *os.File) pidRecord {
	var rec pidRecord
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 4096))
	if err == nil {
		_ = json.Unmarshal(data, &rec)
	}
	return rec
}

// lockHolder is the pid /proc/locks shows holding an flock on f; 0 if none.
func lockHolder(f *os.File) int {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	locks, err := os.Open("/proc/locks")
	if err != nil {
		return 0
	}
	defer locks.Close()
	return flockHolder(locks, fmt.Sprintf("%02x:%02x", unix.Major(st.Dev), unix.Minor(st.Dev)), st.Ino)
}

// flockHolder reads a /proc/locks listing for the holder of the flock on the
// inode ino of device dev ("major:minor" in hex). /proc/locks names the
// filesystem's own device, which is not always the one stat reports: on
// btrfs (a node whose kubelet dir is btrfs, emptyDirs included) stat gives
// the subvolume's anonymous device. So when no lock names dev, the one flock
// on an inode numbered ino is taken as the holder; two such locks are
// ambiguous and name none.
func flockHolder(r io.Reader, dev string, ino uint64) int {
	exact, byIno, matches := 0, 0, 0
	suffix := ":" + strconv.FormatUint(ino, 10)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// "1: FLOCK  ADVISORY  WRITE 4242 00:2f:1234 0 EOF"; a waiter's line
		// has "->" after the number.
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[1] != "FLOCK" || !strings.HasSuffix(fields[5], suffix) {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil {
			continue
		}
		if fields[5] == dev+suffix {
			exact = pid
		}
		byIno, matches = pid, matches+1
	}
	switch {
	case exact != 0:
		return exact
	case matches == 1:
		return byIno
	}
	return 0
}

// isPodAgent reports whether pid is a pod agent running as this uid.
func isPodAgent(pid int) bool {
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	var st syscall.Stat_t
	if syscall.Stat("/proc/"+strconv.Itoa(pid), &st) != nil || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	return strings.Contains(string(cmdline), "pod-agent")
}

// askRelease asks the agent listening on sock to release, within d.
func askRelease(sock, nonce string, d time.Duration) bool {
	conn, err := net.DialTimeout("unix", sock, d)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(d))
	if err := json.NewEncoder(conn).Encode(releaseRequest{Op: "release", Nonce: nonce}); err != nil {
		return false
	}
	var reply releaseReply
	return json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&reply) == nil && reply.OK
}

// serveRelease answers a newer agent's release: the lock goes at once, then
// the session ends (held commands stay with their shepherds).
func (a *agent) serveRelease(l *pidLock) {
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			return
		}
		if !peerIsSelf(conn) {
			_ = conn.Close()
			continue
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(a.opts.TakeoverAsk))
			var req releaseRequest
			if json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req) != nil || req.Op != "release" ||
				subtle.ConstantTimeCompare([]byte(req.Nonce), []byte(l.nonce)) != 1 {
				return
			}
			a.log.Info("a new pod agent took over")
			l.release()
			_ = json.NewEncoder(conn).Encode(releaseReply{OK: true, PID: os.Getpid()})
			a.end()
		}()
	}
}
