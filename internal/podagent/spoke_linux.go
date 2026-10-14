//go:build linux

package podagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// spoke_linux.go relays fleetd's shared-state streams to the pod's spoke,
// the process that keeps the fleet's share (or, on the copy tier, the
// instance's own copy) in sync with the daemon host's hub (internal/
// podshare, internal/sharesync). The main agent starts one for each
// TagSyncCtl stream fleetd opens, as `fleet pod-agent spoke`, and relays the
// stream to it as its control connection; the bulk agent relays each
// TagSyncBulk stream to the running one. They reach it on a socket in a
// folder only the agents' uid may enter; the spoke runs as the share uid.

// spokeStartTimeout bounds a new spoke's start: the lease another spoke
// holds (podshare.SpokeOptions.LeaseWait, 30s), then its tree.
const spokeStartTimeout = 60 * time.Second

// spokeConfig is what the agent hands a spoke it starts (on fd 3).
type spokeConfig struct {
	Spec   podshare.SpokeSpec `json:"spec"`
	Socket string             `json:"socket"`
	Dir    string             `json:"dir"`
	Sync   string             `json:"sync"`
}

// spokeReport is the spoke's answer (on fd 4): its lease epoch once ready,
// or why it could not start.
type spokeReport struct {
	Lease uint64 `json:"lease,omitempty"`
	Error string `json:"error,omitempty"`
}

// spokeSocket is the spoke's socket, its folder made the agents' alone.
func (a *agent) spokeSocket() (string, error) {
	dir, err := privateDir(a.opts.RunDir, podshare.SpokeDirName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, podshare.SpokeSocketName), nil
}

// serveSyncCtl starts the pod's spoke for fleetd's control stream and
// relays the stream to it until either ends; the spoke ends with it.
func (a *agent) serveSyncCtl(s *podwire.Stream) {
	var spec podshare.SpokeSpec
	if err := podwire.DecodeHeader(s.Header, &spec); err != nil {
		s.Penalize()
		podwire.RefuseSock(s, podwire.RefusedUnsupported, err.Error())
		return
	}
	if err := spec.Validate(); err != nil {
		podwire.RefuseSock(s, podwire.RefusedUnsupported, err.Error())
		return
	}
	sock, err := a.spokeSocket()
	if err != nil {
		a.log.Error("the spoke has no private folder", "err", err.Error())
		podwire.RefuseSock(s, podwire.RefusedUnreachable, "the spoke's folder: "+err.Error())
		return
	}
	lease, err := a.startSpoke(spokeConfig{Spec: spec, Socket: sock, Dir: a.opts.ShareDir, Sync: a.opts.ShareSync})
	if err != nil {
		a.log.Error("could not start the share's spoke", "err", err.Error())
		podwire.RefuseSock(s, podwire.RefusedUnreachable, err.Error())
		return
	}
	up, err := dialSpoke(sock, podwire.TagSyncCtl, spec)
	if err != nil {
		a.log.Error("could not reach the share's spoke", "err", err.Error())
		podwire.RefuseSock(s, podwire.RefusedUnreachable, err.Error())
		return
	}
	conn, err := podwire.AcceptSock(s)
	if err != nil {
		_ = up.Close()
		return
	}
	a.log.Info("the share's spoke started", "lease", fmt.Sprint(lease))
	a.spokes.Add(1)
	defer a.spokes.Add(-1)
	splice(conn, up)
}

// serveSyncBulk relays one of fleetd's bulk streams to the running spoke.
func (a *agent) serveSyncBulk(s *podwire.Stream) {
	sock, err := a.spokeSocket()
	if err == nil {
		var up net.Conn
		if up, err = dialSpoke(sock, podwire.TagSyncBulk, struct{}{}); err == nil {
			conn, err := podwire.AcceptSock(s)
			if err != nil {
				_ = up.Close()
				return
			}
			splice(conn, up)
			return
		}
	}
	podwire.RefuseSock(s, podwire.RefusedUnreachable, "no spoke runs in the pod: "+err.Error())
}

// dialSpoke connects to the spoke and names the relay's kind (a podwire
// preamble, header included).
func dialSpoke(sock string, tag podwire.Tag, header any) (net.Conn, error) {
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial the spoke: %w", err)
	}
	if err := podwire.WritePreamble(c, tag, header); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// startSpoke starts a spoke with cfg and waits for it to be ready; it
// returns the spoke's lease epoch. The spoke's warnings go to fleet.log.
func (a *agent) startSpoke(cfg spokeConfig) (uint64, error) {
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		closeAll(cfgR, cfgW)
		return 0, err
	}
	defer repR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(cfgR, cfgW, repW)
		return 0, err
	}
	cmd := &exec.Cmd{
		Path:        a.opts.Exe,
		Args:        append([]string{"fleet"}, a.opts.SpokeArgs...),
		Env:         a.opts.SpokeEnv,
		ExtraFiles:  []*os.File{cfgR, repW},
		Stderr:      errW,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	err = cmd.Start()
	closeAll(cfgR, repW, errW)
	if err != nil {
		closeAll(cfgW, errR)
		return 0, fmt.Errorf("start the spoke: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	go a.forwardSpokeLog(errR)
	err = json.NewEncoder(cfgW).Encode(cfg)
	_ = cfgW.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("configure the spoke: %w", err)
	}
	_ = repR.SetReadDeadline(time.Now().Add(spokeStartTimeout))
	var rep spokeReport
	if err := json.NewDecoder(repR).Decode(&rep); err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("the spoke's report: %w", err)
	}
	if rep.Error != "" {
		return 0, errors.New(rep.Error)
	}
	return rep.Lease, nil
}

// forwardSpokeLog passes the spoke's warnings and errors (JSON log lines on
// its stderr) to fleet.log.
func (a *agent) forwardSpokeLog(r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			a.log.Warn("spoke: " + strings.TrimSpace(sc.Text()))
			continue
		}
		level, _ := rec["level"].(string)
		msg, _ := rec["msg"].(string)
		var kv []string
		for k, v := range rec {
			if k != "level" && k != "msg" && k != "time" {
				kv = append(kv, k, fmt.Sprint(v))
			}
		}
		switch level {
		case "ERROR":
			a.log.Error("spoke: "+msg, kv...)
		case "WARN":
			a.log.Warn("spoke: "+msg, kv...)
		}
	}
}

// RunSpoke is `fleet pod-agent spoke`: it reads its config on fd 3, listens
// on its socket (made the agents' alone before it drops to the share uid),
// and runs the share's spoke (podshare.RunSpoke), reporting on fd 4 once it
// is ready or why it could not be.
func RunSpoke() error {
	cfgF, repF := os.NewFile(3, "spoke-config"), os.NewFile(4, "spoke-report")
	if cfgF == nil || repF == nil {
		return errors.New("pod-agent spoke is started by the pod agent")
	}
	reported := false
	report := func(r spokeReport) {
		if reported {
			return
		}
		reported = true
		_ = json.NewEncoder(repF).Encode(r)
		_ = repF.Close()
	}
	var cfg spokeConfig
	err := json.NewDecoder(io.LimitReader(cfgF, 8<<20)).Decode(&cfg)
	_ = cfgF.Close()
	if err == nil && (cfg.Socket == "" || cfg.Dir == "" || cfg.Sync == "") {
		err = errors.New("incomplete spoke config")
	}
	if err != nil {
		report(spokeReport{Error: err.Error()})
		return err
	}
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := os.Remove(cfg.Socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		report(spokeReport{Error: "the spoke's socket: " + err.Error()})
		return err
	}
	mask := syscall.Umask(0o077)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Socket, Net: "unix"})
	syscall.Umask(mask)
	if err != nil {
		report(spokeReport{Error: "the spoke's socket: " + err.Error()})
		return err
	}
	// Removed by the next spoke or the agent: once the spoke runs as the
	// share uid it may not unlink in the agents' folder.
	ln.SetUnlinkOnClose(false)
	err = podshare.RunSpoke(ctx, podshare.SpokeOptions{
		Spec: cfg.Spec, Dir: cfg.Dir, Sync: cfg.Sync, Listener: ln,
		Ready:  func(lease uint64) { report(spokeReport{Lease: lease}) },
		Logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		report(spokeReport{Error: err.Error()})
		return err
	}
	return nil
}
