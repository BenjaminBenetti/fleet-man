//go:build linux

package podagent

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"golang.org/x/sys/unix"
)

// A shepherd is the process that owns one resumable command: it starts it,
// keeps its PTY and its unacknowledged output, and serves it on a unix socket
// in the held directory to one attachment at a time. It is started in a
// session of its own, so it outlives the agent or exec that started it.

const (
	// shepherdStartTimeout bounds a shepherd's start, report included.
	shepherdStartTimeout = 10 * time.Second
	// shepherdFirstAttach is how long a new shepherd waits for its first
	// attachment before it counts as held.
	shepherdFirstAttach = 30 * time.Second
	heldSuffix          = ".held"
	agentDirName        = "agent"
)

// shepherdConfig is what the starting process hands a shepherd on fd 3.
type shepherdConfig struct {
	ID        string        `json:"id"`
	Plan      *execas.Plan  `json:"plan"`
	Resumable bool          `json:"resumable"`
	Class     execas.Class  `json:"class"`
	User      string        `json:"user"`
	PodUID    string        `json:"pod_uid,omitempty"`
	Socket    string        `json:"socket"`
	HeldDir   string        `json:"held_dir"`
	Grace     time.Duration `json:"grace"`
	MaxHeld   int           `json:"max_held"`
}

// shepherdReport is what a shepherd answers on fd 4 once its command started
// (or did not).
type shepherdReport struct {
	PID   int    `json:"pid,omitempty"`
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// newResumeID is a fresh resume id: 128 random bits.
func newResumeID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// heldSocket is the socket of the held stream with resume id id. The name is
// a hash, so listing the directory does not reveal ids.
func heldSocket(heldDir, id string) string {
	sum := sha256.Sum256([]byte("fleet-held:" + id))
	return filepath.Join(heldDir, hex.EncodeToString(sum[:16])+".sock")
}

// privateDir makes dir (in parent) a directory only this uid can enter, or
// fails: run dirs are emptyDirs anyone in the pod may write to, and a planted
// symlink or a user's own directory there must never be used.
func privateDir(parent, name string) (string, error) {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory of uid %d", dir, os.Geteuid())
	}
	return dir, nil
}

// spawnShepherd starts a shepherd for cfg's command and waits for its report.
func spawnShepherd(o Options, cfg shepherdConfig) (shepherdReport, error) {
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return shepherdReport{}, err
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		closeAll(cfgR, cfgW)
		return shepherdReport{}, err
	}
	defer repR.Close()
	cmd := &exec.Cmd{
		Path:        o.Exe,
		Args:        append([]string{"fleet"}, o.ShepherdArgs...),
		Env:         o.ShepherdEnv,
		ExtraFiles:  []*os.File{cfgR, repW},
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	err = cmd.Start()
	closeAll(cfgR, repW)
	if err != nil {
		_ = cfgW.Close()
		return shepherdReport{}, fmt.Errorf("start shepherd: %w", err)
	}
	// Reaped here while this process lives; reparented to the pod's init
	// once it does not.
	go func() { _ = cmd.Wait() }()
	err = json.NewEncoder(cfgW).Encode(cfg)
	_ = cfgW.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		return shepherdReport{}, fmt.Errorf("configure shepherd: %w", err)
	}
	_ = repR.SetReadDeadline(time.Now().Add(shepherdStartTimeout))
	var rep shepherdReport
	if err := json.NewDecoder(repR).Decode(&rep); err != nil {
		_ = cmd.Process.Kill()
		return shepherdReport{}, fmt.Errorf("shepherd report: %w", err)
	}
	return rep, nil
}

// RunShepherd is `fleet pod-agent shepherd`: it reads its config on fd 3,
// starts the command, reports on fd 4, and serves the command until it is
// done.
func RunShepherd() error {
	cfgF, repF := os.NewFile(3, "shepherd-config"), os.NewFile(4, "shepherd-report")
	if cfgF == nil || repF == nil {
		return errors.New("pod-agent shepherd is started by the pod agent")
	}
	report := func(r shepherdReport) {
		_ = json.NewEncoder(repF).Encode(r)
		_ = repF.Close()
	}
	var cfg shepherdConfig
	err := json.NewDecoder(io.LimitReader(cfgF, 8<<20)).Decode(&cfg)
	_ = cfgF.Close()
	if err == nil && (cfg.Plan == nil || cfg.Socket == "" || !podwire.ValidResumeID(cfg.ID)) {
		err = errors.New("incomplete shepherd config")
	}
	if err != nil {
		report(shepherdReport{Error: err.Error(), Code: execas.ExitFailed})
		return err
	}
	// A hangup must not end the shepherd. It is caught, never ignored: an
	// ignored signal stays ignored in the command and everything it runs
	// (pipelines would print "Broken pipe" were SIGPIPE ignored, and a shell
	// cannot trap a signal ignored on entry), while a caught one is reset for
	// them. SIGPIPE needs nothing: the shepherd's stdio is /dev/null, and Go
	// turns it into EPIPE on every other descriptor.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	syscall.Umask(0o077)

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Socket, Net: "unix"})
	if err != nil {
		report(shepherdReport{Error: "listen: " + err.Error(), Code: execas.ExitFailed})
		return err
	}
	ln.SetUnlinkOnClose(true)
	defer ln.Close()
	p, err := startProc(cfg.Plan, cfg.Resumable)
	if err != nil {
		report(shepherdReport{Error: err.Error(), Code: execas.ExitCode(err)})
		return err
	}
	sh := &shepherd{cfg: cfg, p: p}
	p.onDetach, p.onAttach = sh.detached, sh.attached
	report(shepherdReport{PID: p.pid()})

	first := time.AfterFunc(shepherdFirstAttach, func() {
		p.mu.Lock()
		unattached := p.att == nil && !p.finished
		p.mu.Unlock()
		if unattached {
			if cfg.Resumable {
				sh.detached()
			} else {
				p.giveUp()
			}
		}
	})
	defer first.Stop()
	go sh.accept(ln)
	select {
	case <-p.finishedCh:
	case <-sigs:
		p.giveUp()
		<-p.finishedCh
	}
	sh.attached()
	return nil
}

type shepherd struct {
	cfg shepherdConfig
	p   *proc

	mu     sync.Mutex
	marker string
	grace  *time.Timer
}

func (sh *shepherd) accept(ln *net.UnixListener) {
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		if !peerIsSelf(conn) {
			_ = conn.Close()
			continue
		}
		go sh.serve(conn)
	}
}

// serve takes one attachment: a re-attach header naming this command.
func (sh *shepherd) serve(conn *net.UnixConn) {
	_ = conn.SetReadDeadline(time.Now().Add(podwire.HeaderDeadline))
	tag, raw, err := podwire.ReadPreamble(conn)
	if err != nil || tag != podwire.TagExec {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	w := podwire.NewFrameWriter(conn)
	var h podwire.ExecHeader
	if podwire.DecodeHeader(raw, &h) != nil || !sh.matches(h) {
		_ = w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{Gone: true})
		_ = conn.Close()
		return
	}
	att := &attachment{r: podwire.NewFrameReader(conn), w: w, c: conn}
	pid := sh.p.pid()
	err = sh.p.attach(att, h.Stdout, h.Stderr, func(stdin int64) any {
		return podwire.ResumeReply{Stdin: stdin, Stdout: h.Stdout, Stderr: h.Stderr, PID: pid}
	})
	if err != nil {
		_ = w.WriteJSON(podwire.FrameOpen, podwire.OpenReply{Error: err.Error(), Gone: true})
		_ = conn.Close()
	}
}

// matches reports whether h re-attaches this command: its resume id, the
// same exec user, and the same pod.
func (sh *shepherd) matches(h podwire.ExecHeader) bool {
	if subtle.ConstantTimeCompare([]byte(h.Resume), []byte(sh.cfg.ID)) != 1 {
		return false
	}
	if h.Class != sh.cfg.Class || h.User != sh.cfg.User {
		return false
	}
	return h.PodUID == "" || sh.cfg.PodUID == "" || h.PodUID == sh.cfg.PodUID
}

// detached records the command as held: a marker in the held directory (the
// pod-wide count and age order), a grace timer, and room made by ending the
// oldest held commands past MaxHeld.
func (sh *shepherd) detached() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.marker != "" {
		return
	}
	name := fmt.Sprintf("%020d-%s%s", time.Now().UnixNano(), strings.TrimSuffix(filepath.Base(sh.cfg.Socket), ".sock"), heldSuffix)
	sh.marker = filepath.Join(sh.cfg.HeldDir, name)
	_ = os.WriteFile(sh.marker, []byte(strconv.Itoa(os.Getpid())), 0o600)
	sh.grace = time.AfterFunc(sh.cfg.Grace, sh.p.giveUp)
	endOldestHeld(sh.cfg.HeldDir, sh.cfg.MaxHeld, sh.marker)
}

// attached clears what detached recorded. It runs under the proc's lock.
func (sh *shepherd) attached() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.grace != nil {
		sh.grace.Stop()
		sh.grace = nil
	}
	if sh.marker != "" {
		_ = os.Remove(sh.marker)
		sh.marker = ""
	}
}

// endOldestHeld ends the oldest held commands in heldDir beyond max (never
// self's): SIGTERM to each one's shepherd, which aborts its command.
func endOldestHeld(heldDir string, max int, self string) {
	entries, err := os.ReadDir(heldDir)
	if err != nil {
		return
	}
	var markers []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), heldSuffix) {
			markers = append(markers, filepath.Join(heldDir, e.Name()))
		}
	}
	sort.Strings(markers)
	for i := 0; i < len(markers)-max; i++ {
		if markers[i] == self {
			continue
		}
		data, err := os.ReadFile(markers[i])
		pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || perr != nil || !isShepherd(pid) {
			// Stale: its shepherd is gone.
			_ = os.Remove(markers[i])
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// isShepherd reports whether pid is a live shepherd of this uid.
func isShepherd(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	var st syscall.Stat_t
	if syscall.Stat("/proc/"+strconv.Itoa(pid), &st) != nil || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	return strings.Contains(string(cmdline), "shepherd")
}

// peerIsSelf reports whether conn's peer runs as this process's uid: only
// fleet's own processes may reach a held command or the agent.
func peerIsSelf(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return false
	}
	return cred.Uid == uint32(os.Geteuid())
}
