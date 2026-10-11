//go:build linux

package podshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
)

// SpokeOptions configure RunSpoke.
type SpokeOptions struct {
	Spec SpokeSpec
	// Dir is the tree's root (MountDir), inside Sync (SyncDir).
	Dir  string
	Sync string
	// Listener is where the agents relay fleetd's streams to: the spoke's
	// socket, in a folder only the agents' uid may enter. RunSpoke closes
	// it.
	Listener net.Listener
	// Ready hears the spoke's lease epoch once the spoke is ready for its
	// control connection.
	Ready func(lease uint64)
	// LeaseWait bounds the wait for the lease another spoke holds (one in
	// another of the fleet's pods, or this pod's previous one, ending): 30s.
	LeaseWait time.Duration
	// IdleWait bounds the wait for the control connection once ready: 30s.
	IdleWait time.Duration
	Logger   *slog.Logger
}

// RunSpoke is `fleet pod-agent spoke`: the tree's one spoke. As root it
// opens the tree's root (checking SyncDir is root's alone and the tree a
// mount of its own inside it) and changes into it, then runs as the share
// uid, never dumpable (so the pod's users, the same uid, cannot reach its
// files through /proc), takes the lease (waiting for one another spoke
// holds), opens its Tree over RootsDir with its state in StateDir, says it
// is ready, and serves the first control connection the agents relay
// (sharesync.Spoke) and the bulk connections after it, until the control
// connection ends.
func RunSpoke(ctx context.Context, o SpokeOptions) error {
	if o.LeaseWait <= 0 {
		o.LeaseWait = 30 * time.Second
	}
	if o.IdleWait <= 0 {
		o.IdleWait = 30 * time.Second
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	defer o.Listener.Close()
	if err := o.Spec.Validate(); err != nil {
		return err
	}
	if err := enterTree(o.Sync, o.Dir); err != nil {
		return err
	}
	if err := dropTo(o.Spec.UID, o.Spec.GID); err != nil {
		return err
	}
	lease, epoch, err := takeLease(ctx, LeaseFile, o.Spec.Pod, o.LeaseWait)
	if err != nil {
		return err
	}
	defer lease.Close()

	var roots []sharesync.TreeRoot
	for _, r := range o.Spec.Roots {
		roots = append(roots, sharesync.TreeRoot{Name: r.Name, Dir: r.Dir(), Reserved: r.Reserved, Skip: r.Skip, Only: r.Only})
	}
	tree, err := sharesync.OpenTree(roots, sharesync.TreeOptions{StateDir: StateDir, FullReason: "share full", Logger: log})
	if err != nil {
		return err
	}
	defer tree.Close()
	side := sharesync.SideOptions{NoWatch: o.Spec.NoWatch, Logger: log}
	if o.Spec.Rescan != "" {
		side.Rescan, _ = time.ParseDuration(o.Spec.Rescan)
	}
	if o.Ready != nil {
		o.Ready(epoch)
	}
	return serveSpoke(ctx, o.Listener, tree, sharesync.SpokeOptions{
		Info: sharesync.SideInfo{ID: "pod:" + o.Spec.Pod}, Side: side, Lease: epoch, Logger: log,
	}, o.IdleWait, log)
}

// serveSpoke accepts the agents' relays: the first control connection runs
// the spoke, bulk connections are its; it returns when the control
// connection ends.
func serveSpoke(ctx context.Context, ln net.Listener, tree *sharesync.Tree, so sharesync.SpokeOptions, idle time.Duration, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conns := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case conns <- c:
			case <-ctx.Done():
				_ = c.Close()
				return
			}
		}
	}()
	defer ln.Close()
	var (
		mu    sync.Mutex
		spoke *sharesync.Spoke
		done  = make(chan error, 1)
		bulks sync.WaitGroup
	)
	defer bulks.Wait()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			// The control connection ended: fleetd closed it, or the agent
			// relaying it went. Either way the spoke's work is done.
			cancel()
			log.Info("spoke: the control connection ended", "why", err)
			return nil
		case <-idleT.C:
			mu.Lock()
			started := spoke != nil
			mu.Unlock()
			if !started {
				return errors.New("no control connection came")
			}
		case c := <-conns:
			_ = c.SetReadDeadline(time.Now().Add(podwire.HeaderDeadline))
			tag, _, err := podwire.ReadPreamble(c)
			_ = c.SetReadDeadline(time.Time{})
			if err != nil {
				log.Warn("spoke: a relay without its preamble", "err", err)
				_ = c.Close()
				continue
			}
			mu.Lock()
			sp := spoke
			mu.Unlock()
			switch {
			case tag == podwire.TagSyncCtl && sp == nil:
				sp = sharesync.NewSpoke(c, tree, so)
				mu.Lock()
				spoke = sp
				mu.Unlock()
				go func() { done <- sp.Run(ctx) }()
			case tag == podwire.TagSyncBulk && sp != nil:
				bulks.Add(1)
				go func() {
					defer bulks.Done()
					if err := sp.ServeBulk(ctx, c); err != nil && ctx.Err() == nil {
						log.Warn("spoke: a bulk connection", "err", err)
					}
				}()
			default:
				log.Warn("spoke: refused a relay", "kind", tag.String(), "running", sp != nil)
				_ = c.Close()
			}
		}
	}
}

// enterTree changes into the tree's root, dir, inside syncDir. As root,
// syncDir must be root's alone: no user of the pod made it (or anything in
// it, the tree's mount included). Both are opened without following a
// symlink.
func enterTree(syncDir, dir string) error {
	p := syncDir + "/"
	if len(dir) <= len(p) || dir[:len(p)] != p {
		return fmt.Errorf("the share's root %s is not inside %s", dir, syncDir)
	}
	sfd, err := unix.Open(syncDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("the share's mount folder %s: %w", syncDir, err)
	}
	defer unix.Close(sfd)
	var sst unix.Stat_t
	if err := unix.Fstat(sfd, &sst); err != nil {
		return err
	}
	if os.Geteuid() == 0 && (sst.Uid != 0 || sst.Mode&0o077 != 0) {
		return fmt.Errorf("the share's mount folder %s is not root's alone (uid %d, mode %o)", syncDir, sst.Uid, sst.Mode&0o7777)
	}
	fd, err := unix.Openat(sfd, dir[len(p):], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("the share's root %s: %w", dir, err)
	}
	defer unix.Close(fd)
	return unix.Fchdir(fd)
}

// dropTo makes the process uid:gid for good (all its threads), with no
// supplementary groups, and never dumpable: a process that changed its ids
// already is not, and the explicit prctl keeps it so. -1 (or not root)
// keeps the ids.
func dropTo(uid, gid int) error {
	if uid < 0 || os.Geteuid() != 0 || uid == 0 {
		return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	}
	// syscall's, not x/sys/unix's: they change every thread of the
	// process (x/sys's Setgroups changes only the calling one).
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("dropping supplementary groups: %w", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("becoming gid %d: %w", gid, err)
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("becoming uid %d: %w", uid, err)
	}
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// leaseRecord is the lease file's content.
type leaseRecord struct {
	Epoch uint64    `json:"epoch"`
	Pod   string    `json:"pod,omitempty"`
	At    time.Time `json:"at"`
}

// takeLease locks the lease file name (waiting up to wait for another
// spoke's lock), raises its epoch and returns the file, whose lock is held
// until it is closed, with the new epoch.
func takeLease(ctx context.Context, name, pod string, wait time.Duration) (*os.File, uint64, error) {
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("the spoke's lease: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, 0, fmt.Errorf("locking the spoke's lease: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, 0, fmt.Errorf("another spoke holds the share's lease (%s) after %s", name, wait)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	var rec leaseRecord
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &rec) // a torn or foreign lease counts from 0
	}
	rec.Epoch++
	rec.Pod, rec.At = pod, time.Now().UTC()
	out, _ := json.Marshal(rec)
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if _, err := f.WriteAt(append(out, '\n'), 0); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, rec.Epoch, nil
}
