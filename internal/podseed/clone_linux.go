//go:build linux

package podseed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

// clone_linux.go is an OpClone: the clone's disk made from its source's. A
// copier (Clone.Source set) copies the source disk's instance dirs over first;
// a disk restored from a snapshot or a CSI clone already holds them. Then the
// clone's own fix-ups, as a new docker container from a committed image gets
// them: its own hostname and resolv.conf, its volumes reset to what the image
// copies up (docker commit drops volumes), and none of the source's PID 1
// logs, share-sync state or trashed generations.

// cloneImageRoot is the image a clone's volumes copy up from: the job pod's
// own root, the instance's image (tests point it elsewhere).
var cloneImageRoot = "/"

// cloneTops are the instance disk's dirs a copier copies: the root generation,
// the workspace, the fleet binaries and the manifest. vol/ is reset and sync/
// is the source's alone.
var cloneTops = []string{DirRoot, DirRootExtra, DirWorkspace, DirFleet, DirMeta}

// cloneExcluded names what a copier never copies, relative to the disk: the
// source's PID 1 logs and trashed manifests.
func cloneExcluded(rel string) bool {
	switch {
	case strings.HasPrefix(rel, FleetLogDir+"/"):
		return true
	case strings.HasPrefix(rel, DirMeta+"/"):
		return trashManifestName.MatchString(strings.TrimPrefix(rel, DirMeta+"/"))
	}
	return false
}

// cloneRun is one OpClone.
type cloneRun struct {
	spec  JobSpec
	clone CloneSpec
	srv   *jobboot.Server
	logf  func(string, ...any)
	stall time.Duration

	pvc   string
	stats copyStats
	m     Manifest
	mu    sync.Mutex
	phase string
}

func (r *cloneRun) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

func (r *cloneRun) progress(done bool) {
	r.mu.Lock()
	phase := r.phase
	r.mu.Unlock()
	r.srv.Progress(jobboot.Progress{Phase: "clone", Detail: phase,
		Bytes: r.stats.bytes.Load(), Files: r.stats.files.Load(), Done: done})
}

func (r *cloneRun) run(ctx context.Context) jobOutcome {
	r.pvc = r.spec.PVC
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	tctx, stopTicks := context.WithCancel(ctx)
	defer stopTicks()
	go func() {
		for {
			select {
			case <-tick.C:
				r.progress(false)
			case <-tctx.Done():
				return
			}
		}
	}()

	if r.clone.Source != "" {
		if err := r.copyDisk(ctx); err != nil {
			return jobOutcome{err: err}
		}
	}
	m, err := ReadManifest(filepath.Join(r.pvc, ManifestFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return jobOutcome{err: ErrNoInstance}
	case err != nil:
		return jobOutcome{err: fmt.Errorf("the clone's root manifest: %w", err)}
	case m.Gen != r.spec.Gen:
		return jobOutcome{err: fmt.Errorf("the disk holds root generation %d, the clone is of generation %d", m.Gen, r.spec.Gen)}
	}
	r.m = m
	if err := r.fixUp(ctx); err != nil {
		return jobOutcome{err: err}
	}
	r.setPhase("sync")
	if err := syncFS(r.pvc); err != nil {
		return jobOutcome{err: fmt.Errorf("sync the clone's disk: %w", err)}
	}
	r.setPhase("done")
	r.progress(true)
	data, sha := r.m.encode()
	if err := writeDurable(filepath.Join(r.pvc, ManifestFile), data, 0o644); err != nil {
		return jobOutcome{err: fmt.Errorf("write the manifest: %w", err)}
	}
	return jobOutcome{bytes: r.stats.bytes.Load(), output: data, sha: sha}
}

// copyDisk copies the source disk's instance dirs onto the clone's. A live
// source gets passes until one changes little (or Passes ran), then the final
// delta between the two barriers, while fleetd holds the source still (frozen
// or stopped) or, where it cannot, while it runs on.
func (r *cloneRun) copyDisk(ctx context.Context) error {
	src := r.clone.Source
	sm, err := ReadManifest(filepath.Join(src, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the source disk: %w", ErrNoInstance)
	}
	if err != nil {
		return fmt.Errorf("the source disk's root manifest: %w", err)
	}
	if sm.Gen != r.spec.Gen {
		return fmt.Errorf("the source disk holds root generation %d, not %d: it was rebuilt meanwhile", sm.Gen, r.spec.Gen)
	}
	s, err := newSyncer(src, r.pvc, r.clone.Live, r.clone.Workers, &r.stats)
	if err != nil {
		return err
	}
	s.exclude = cloneExcluded
	if !r.clone.Live {
		r.setPhase("copy")
		if _, err := s.pass(ctx, cloneTops); err != nil {
			return fmt.Errorf("copy the source disk: %w", err)
		}
		return r.finishCopy(s)
	}
	passes := r.clone.Passes
	if passes <= 0 {
		passes = DefaultClonePasses
	}
	for i := 1; i <= passes; i++ {
		r.setPhase(fmt.Sprintf("live pass %d", i))
		start := time.Now()
		changed, err := s.pass(ctx, cloneTops)
		if err != nil {
			return fmt.Errorf("copy the running source (pass %d): %w", i, err)
		}
		r.logf("live pass %d: %d entries changed in %s", i, changed, time.Since(start).Round(time.Millisecond))
		if i > 1 && (changed < 64 || time.Since(start) < 5*time.Second) {
			break
		}
	}
	r.setPhase("final delta")
	rel, err := r.srv.Barrier(ctx, BarrierFinal, "ready for the final delta", r.stall)
	if err != nil {
		return fmt.Errorf("waiting for fleetd before the final delta: %w", err)
	}
	if !rel.OK {
		return fmt.Errorf("fleetd did not get the source ready for the final delta: %s", rel.Error)
	}
	start := time.Now()
	changed, err := s.pass(ctx, cloneTops)
	took := time.Since(start)
	// Whatever the final pass did, the source goes on.
	rel2, berr := r.srv.Barrier(ctx, BarrierCopied, fmt.Sprintf("final delta: %d entries in %s", changed, took.Round(time.Millisecond)), r.stall)
	if err != nil {
		return fmt.Errorf("copy the final delta: %w", err)
	}
	if berr != nil {
		return fmt.Errorf("waiting for fleetd after the final delta: %w", berr)
	}
	if !rel2.OK {
		return fmt.Errorf("fleetd after the final delta: %s", rel2.Error)
	}
	r.logf("final delta: %d entries changed in %s", changed, took.Round(time.Millisecond))
	return r.finishCopy(s)
}

func (r *cloneRun) finishCopy(s *syncer) error {
	r.setPhase("self-check")
	err := s.check()
	for _, w := range s.warnings {
		r.m.addWarning("%s", w)
	}
	if err != nil {
		return fmt.Errorf("clone %w", err)
	}
	return nil
}

// fixUp makes the copied disk the clone's.
func (r *cloneRun) fixUp(ctx context.Context) error {
	r.setPhase("fix-ups")
	entries, err := os.ReadDir(r.pvc)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if trashName.MatchString(e.Name()) || partialName.MatchString(e.Name()) || e.Name() == DirSync {
			if err := removeTree(filepath.Join(r.pvc, e.Name())); err != nil {
				return fmt.Errorf("delete the source's %s: %w", e.Name(), err)
			}
		}
	}
	if metas, err := os.ReadDir(filepath.Join(r.pvc, DirMeta)); err == nil {
		for _, e := range metas {
			if trashManifestName.MatchString(e.Name()) {
				if err := os.Remove(filepath.Join(r.pvc, DirMeta, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	// The source's PID 1 output is the source's.
	if err := removeTree(filepath.Join(r.pvc, FleetLogDir)); err != nil {
		return err
	}
	for _, dir := range []string{FleetBinDir, FleetLogDir, DirFleet + "/spec", DirFleet + "/run", DirMeta, DirWorkspace} {
		mode := os.FileMode(0o755)
		if dir == FleetLogDir {
			mode = 0o700
		}
		if err := mkdirOwned(filepath.Join(r.pvc, dir), mode); err != nil {
			return fmt.Errorf("lay out the clone's disk: %w", err)
		}
	}
	resolv, err := os.ReadFile(liveResolvConf)
	if err != nil {
		return fmt.Errorf("read the job pod's resolv.conf: %w", err)
	}
	if err := writePlaceholder(filepath.Join(r.pvc, ResolvConfFile), resolv); err != nil {
		return err
	}
	if err := writePlaceholder(filepath.Join(r.pvc, HostnameFile), []byte(r.clone.Hostname+"\n")); err != nil {
		return err
	}
	return r.resetVolumes(ctx)
}

// resetVolumes makes vol/ what a new container's volumes are: each copied up
// from the image (this job's own root, the instance's image) at its target,
// as the source's seed left the image (its uid remap), or empty.
func (r *cloneRun) resetVolumes(ctx context.Context) error {
	r.setPhase("volumes")
	vol := filepath.Join(r.pvc, DirVolumes)
	if err := discard(vol, filepath.Join(r.pvc, "."+DirVolumes+".partial")); err != nil {
		return err
	}
	if err := mkdirOwned(vol, 0o755); err != nil {
		return err
	}
	r.m.Volumes = nil
	if len(r.clone.Volumes) == 0 {
		return nil
	}
	mounts, err := sourceMounts(cloneImageRoot)
	if err != nil {
		return err
	}
	tops, _, err := classifyTops(cloneImageRoot, mounts)
	if err != nil {
		return err
	}
	plan := planRoot(tops, SeedSpec{MountDirs: r.clone.MountDirs, Hostname: r.clone.Hostname})
	seed := &seedRun{
		spec: r.spec, seed: SeedSpec{Volumes: r.clone.Volumes, Workers: r.clone.Workers, Hostname: r.clone.Hostname},
		srv: r.srv, logf: r.logf, pvc: r.pvc, src: cloneImageRoot, m: r.m,
	}
	if err := seed.copyVolumes(ctx, mounts, plan.mountpoints); err != nil {
		return err
	}
	r.m.Volumes = seed.m.Volumes
	r.m.Warnings = seed.m.Warnings
	return nil
}
