//go:build linux

package podseed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"golang.org/x/sys/unix"
)

// Paths the job reads from the seed pod itself; tests point them elsewhere.
var (
	// liveResolvConf is the seed pod's resolv.conf: the cluster DNS config an
	// instance pod in the same namespace gets too.
	liveResolvConf = "/etc/resolv.conf"
	// selfExe is the running binary, installed into fleet/bin.
	selfExe = "/proc/self/exe"
)

// kubeletFiles are the files the kubelet and the runtime mount into /etc. The
// seed writes an empty placeholder for each into every generation's root/etc:
// a read-only /etc could not get its mountpoints otherwise.
var kubeletFiles = []string{"hosts", "hostname", "resolv.conf"}

// RunJob is `fleet pod-job --spec specPath`: it runs the job, records its
// result on the PVC, in the termination log and over the job socket, and
// returns the container's exit code. log gets its progress lines (the Job
// pod's log).
func RunJob(ctx context.Context, specPath string, log io.Writer) int {
	logf := func(format string, args ...any) {
		fmt.Fprintf(log, "fleet pod-job: "+format+"\n", args...)
	}
	spec, err := LoadJobSpec(specPath)
	if err != nil {
		logf("%v", err)
		_ = jobboot.WriteTermination("", jobboot.Termination{Error: err.Error()})
		return jobboot.ExitBadSpec
	}
	fail := func(code int, err error) int {
		logf("%v", err)
		_ = jobboot.WriteTermination(spec.TerminationLog, jobboot.Termination{Error: err.Error()})
		return code
	}
	if spec.BinarySHA256 != "" {
		sha, err := FileSHA256(selfExe)
		if err != nil {
			return fail(jobboot.ExitBadSpec, fmt.Errorf("hash this binary: %w", err))
		}
		if sha != spec.BinarySHA256 {
			return fail(jobboot.ExitBadSpec, fmt.Errorf("this binary's sha256 is %s, the spec names %s", sha, spec.BinarySHA256))
		}
	}
	wantsWorkspace := spec.Op == OpSeed && spec.Seed.Workspace != nil
	srv, err := jobboot.Listen(jobboot.Paths{Dir: spec.Tools}.Socket(), jobboot.ServerOptions{
		Op: string(spec.Op), Gen: spec.Gen, Workspace: wantsWorkspace, Logf: logf,
	})
	if err != nil {
		return fail(jobboot.ExitFailed, err)
	}
	defer srv.Close()
	stall, hold := spec.durations()

	logf("%s generation %d on %s", spec.Op, spec.Gen, spec.PVC)
	start := time.Now()
	var out jobOutcome
	switch spec.Op {
	case OpSeed:
		run := &seedRun{spec: spec, seed: *spec.Seed, srv: srv, logf: logf, stall: stall}
		out = run.run(ctx)
	case OpRollback:
		out = outcomeOf(rollbackGeneration(spec.PVC, spec.Gen), spec.PVC)
	case OpCommit:
		out = outcomeOf(commitGeneration(spec.PVC, spec.Gen), spec.PVC)
	}

	term := jobboot.Termination{OK: out.err == nil, SHA256: out.sha, Bytes: out.bytes}
	result := jobboot.Result{OK: out.err == nil, Bytes: out.bytes, Output: out.output}
	code := 0
	if out.err != nil {
		term.Error, result.Error = out.err.Error(), out.err.Error()
		code = jobboot.ExitFailed
		if errors.Is(out.err, jobboot.ErrStalled) {
			code = jobboot.ExitStalled
		}
		logf("failed after %s: %v", time.Since(start).Round(time.Millisecond), out.err)
	} else {
		logf("done in %s", time.Since(start).Round(time.Millisecond))
	}
	if err := jobboot.WriteTermination(spec.TerminationLog, term); err != nil {
		logf("write the termination message: %v", err)
	}
	if err := srv.Finish(ctx, result, hold); err != nil {
		logf("result not collected: %v", err)
	}
	return code
}

// jobOutcome is how a job ended.
type jobOutcome struct {
	err    error
	bytes  int64
	output []byte
	sha    string
}

// outcomeOf is a rollback or commit's outcome: the PVC's current manifest is
// its output, so fleetd learns the generation it now holds.
func outcomeOf(err error, pvc string) jobOutcome {
	if err != nil {
		return jobOutcome{err: err}
	}
	data, err := os.ReadFile(filepath.Join(pvc, ManifestFile))
	if err != nil {
		return jobOutcome{}
	}
	return jobOutcome{output: data, sha: sha256Hex(data)}
}

// seedRun is one OpSeed.
type seedRun struct {
	spec  JobSpec
	seed  SeedSpec
	srv   *jobboot.Server
	logf  func(string, ...any)
	stall time.Duration

	pvc, src string
	stats    copyStats
	m        Manifest
	mu       sync.Mutex
	phase    string
}

func (r *seedRun) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

func (r *seedRun) progress(done bool) {
	r.mu.Lock()
	phase := r.phase
	r.mu.Unlock()
	r.srv.Progress(jobboot.Progress{Phase: "copy", Detail: phase,
		Bytes: r.stats.bytes.Load(), Files: r.stats.files.Load(), Done: done})
}

func (r *seedRun) run(ctx context.Context) jobOutcome {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.pvc = r.spec.PVC
	r.src = r.seed.Source
	if r.src == "" {
		r.src = "/"
	}
	r.m = Manifest{V: ManifestVersion, Gen: r.spec.Gen, SingleUID: os.Geteuid() != 0}

	// The workspace arrives over the socket while the root copies.
	var wsWait sync.WaitGroup
	var wsErr error
	var wsStats WorkspaceStats
	if r.seed.Workspace != nil {
		wsWait.Add(1)
		go func() {
			defer wsWait.Done()
			wsStats, wsErr = r.receiveWorkspace(ctx)
			if wsErr != nil {
				cancel(wsErr)
			}
		}()
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-tick.C:
				r.progress(false)
			case <-ctx.Done():
				return
			}
		}
	}()

	err := r.seedRoot(ctx)
	if err != nil {
		cancel(err)
	}
	wsWait.Wait()
	switch {
	case wsErr != nil && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, wsErr)):
		return jobOutcome{err: fmt.Errorf("workspace: %w", wsErr)}
	case err != nil:
		if cause := context.Cause(ctx); errors.Is(err, context.Canceled) && cause != nil && cause != err {
			err = cause
		}
		return jobOutcome{err: err}
	}
	r.setPhase("image root")
	r.progress(true)

	r.setPhase("sync")
	if err := syncFS(r.pvc); err != nil {
		return jobOutcome{err: fmt.Errorf("sync the instance disk: %w", err)}
	}
	r.m.SeededAt = time.Now().UTC()
	r.m.Files, r.m.Bytes, r.m.Hardlinks = r.stats.files.Load(), r.stats.bytes.Load(), r.stats.hardlinks.Load()
	r.m.Capabilities, r.m.Setuid = r.stats.caps.Load(), r.stats.setuid.Load()
	data, sha := r.m.encode()
	if err := writeDurable(filepath.Join(r.pvc, ManifestFile), data, 0o644); err != nil {
		return jobOutcome{err: fmt.Errorf("write the manifest: %w", err)}
	}
	return jobOutcome{bytes: r.m.Bytes + wsStats.Bytes, output: data, sha: sha}
}

// seedRoot does everything but the workspace.
func (r *seedRun) seedRoot(ctx context.Context) error {
	if err := r.prepare(); err != nil {
		return err
	}
	mounts, err := sourceMounts(r.src)
	if err != nil {
		return err
	}
	tops, topLinks, err := classifyTops(r.src, mounts)
	if err != nil {
		return err
	}
	plan := planRoot(tops, r.seed)
	for _, w := range plan.warnings {
		r.m.addWarning("%s", w)
	}
	r.m.Dirs, r.m.RootExtra = plan.dirs, plan.extra

	for _, top := range plan.dirs {
		if err := r.copyTop(ctx, top, mounts, plan.mountpoints); err != nil {
			return err
		}
	}
	if err := r.copyVolumes(ctx, mounts, plan.mountpoints); err != nil {
		return err
	}
	for _, top := range plan.extra {
		if err := mkdirOwned(filepath.Join(r.pvc, DirRootExtra, top), 0o755); err != nil {
			return fmt.Errorf("create root-extra/%s: %w", top, err)
		}
	}
	g := genRoot{pvc: r.pvc, dirs: setOf(plan.dirs), extra: setOf(plan.extra), topLinks: topLinks}
	if err := r.writeKubeletFiles(g); err != nil {
		return err
	}
	if err := r.remap(g); err != nil {
		return err
	}
	return r.installFleet(g)
}

// prepare lays out the PVC and gets the generation's dirs ready: after a
// reset for a rebuild, else after discarding what an earlier attempt of this
// same seed left.
func (r *seedRun) prepare() error {
	for _, dir := range []string{DirVolumes, DirWorkspace, FleetBinDir, FleetLogDir,
		DirFleet + "/spec", DirFleet + "/run", DirMeta} {
		if err := mkdirOwned(filepath.Join(r.pvc, dir), 0o755); err != nil {
			return fmt.Errorf("lay out the instance disk: %w", err)
		}
	}
	if r.seed.Reset {
		r.setPhase("reset")
		if err := resetGeneration(r.pvc, r.seed.PrevGen, r.spec.Gen); err != nil {
			return fmt.Errorf("reset generation %d: %w", r.seed.PrevGen, err)
		}
	} else {
		if m, err := ReadManifest(filepath.Join(r.pvc, ManifestFile)); err == nil || errors.Is(err, ErrNewerManifest) {
			if m.Gen != r.spec.Gen {
				return fmt.Errorf("the instance disk holds root generation %d; seeding generation %d over it needs a reset", m.Gen, r.spec.Gen)
			}
		}
		if err := removePartials(r.pvc); err != nil {
			return err
		}
		for _, dir := range persistedDirs {
			if err := discard(filepath.Join(r.pvc, dir), filepath.Join(r.pvc, partialDir(dir, r.spec.Gen))); err != nil {
				return err
			}
		}
		if err := removeIfExists(filepath.Join(r.pvc, ManifestFile)); err != nil {
			return err
		}
	}
	for _, dir := range persistedDirs {
		if err := mkdirOwned(filepath.Join(r.pvc, dir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// copyTop copies one top-level dir of the image into root/, again with GNU cp
// when the Go copier fails on it or its self-check does.
func (r *seedRun) copyTop(ctx context.Context, top string, mounts, mountpoints map[string]bool) error {
	r.setPhase("/" + top)
	src := filepath.Join(r.src, top)
	dst := filepath.Join(r.pvc, DirRoot, top)
	c, err := newCopier(r.src, mounts, mountpoints, r.seed.Workers, &r.stats)
	if err != nil {
		return err
	}
	err = c.copyTree(ctx, src, dst)
	if err == nil {
		err = c.selfCheck()
	}
	for _, w := range c.warnings {
		r.m.addWarning("%s", w)
	}
	if err == nil {
		return nil
	}
	cp, ok := gnuCP()
	if !ok || !fallbackable(err) || ctx.Err() != nil {
		return fmt.Errorf("copy /%s: %w", top, err)
	}
	r.logf("copy /%s: %v; copying it again with cp", top, err)
	if derr := discard(dst, filepath.Join(r.pvc, DirRoot, "."+top+".partial")); derr != nil {
		return derr
	}
	if cerr := cpTree(ctx, cp, src, dst, c.singleUID); cerr != nil {
		return fmt.Errorf("copy /%s: %w (then %w)", top, err, cerr)
	}
	c.resetChecks()
	if serr := c.scan(src, dst); serr != nil {
		return fmt.Errorf("check /%s after cp: %w", top, serr)
	}
	if serr := c.selfCheck(); serr != nil {
		return fmt.Errorf("copy /%s: %w (cp too: %w)", top, err, serr)
	}
	r.m.Fallback = append(r.m.Fallback, top)
	return nil
}

// copyVolumes seeds the volumes new to this disk: copied up from the image at
// their target as docker fills a new named volume, else empty. A volume this
// disk already has is the user's data, kept across rebuilds.
func (r *seedRun) copyVolumes(ctx context.Context, mounts, mountpoints map[string]bool) error {
	for _, v := range r.seed.Volumes {
		dst := filepath.Join(r.pvc, DirVolumes, v.Name)
		if exists(dst) {
			continue
		}
		r.setPhase("volume " + v.Name)
		tmp := filepath.Join(r.pvc, DirVolumes, "."+v.Name+".partial")
		if err := removeTree(tmp); err != nil {
			return err
		}
		src := filepath.Join(r.src, strings.TrimPrefix(v.Target, "/"))
		fi, err := os.Stat(src)
		if v.CopyUp && err == nil && fi.IsDir() {
			// The volume's own target is a mountpoint of the root copy; its
			// content is exactly what this copy is for.
			own := map[string]bool{}
			for p := range mountpoints {
				if p != v.Target {
					own[p] = true
				}
			}
			c, err := newCopier(r.src, mounts, own, r.seed.Workers, &r.stats)
			if err != nil {
				return err
			}
			if err := c.copyTree(ctx, src, tmp); err != nil {
				return fmt.Errorf("copy up volume %s: %w", v.Name, err)
			}
			for _, w := range c.warnings {
				r.m.addWarning("%s", w)
			}
		} else if err := mkdirOwned(tmp, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return fmt.Errorf("seed volume %s: %w", v.Name, err)
		}
		r.m.Volumes = append(r.m.Volumes, v.Name)
	}
	return nil
}

// writeKubeletFiles writes the placeholders in root/etc and the files the pod
// mounts over the kubelet's resolv.conf and hostname.
func (r *seedRun) writeKubeletFiles(g genRoot) error {
	r.setPhase("kubelet files")
	if etc, err := g.resolveDir("/etc"); err != nil {
		r.m.addWarning("no /etc in the image: %v", err)
	} else {
		for _, name := range kubeletFiles {
			if err := writePlaceholder(filepath.Join(etc, name), nil); err != nil {
				return fmt.Errorf("placeholder /etc/%s: %w", name, err)
			}
		}
	}
	resolv, err := os.ReadFile(liveResolvConf)
	if err != nil {
		return fmt.Errorf("read the seed pod's resolv.conf: %w", err)
	}
	if err := writePlaceholder(filepath.Join(r.pvc, ResolvConfFile), resolv); err != nil {
		return err
	}
	return writePlaceholder(filepath.Join(r.pvc, HostnameFile), []byte(r.seed.Hostname+"\n"))
}

// remap does the share-uid remap in the new /etc.
func (r *seedRun) remap(g genRoot) error {
	rm := r.seed.Remap
	if rm == nil {
		return nil
	}
	r.setPhase("remap")
	if os.Geteuid() != 0 {
		r.m.addWarning("no uid remap without root: %s keeps the image's uid", rm.User)
		return nil
	}
	passwdPath, err := g.resolve("/etc/passwd")
	if err != nil {
		r.m.addWarning("uid remap: %v", err)
		return nil
	}
	groupPath, err := g.resolve("/etc/group")
	if err != nil {
		r.m.addWarning("uid remap: %v", err)
		return nil
	}
	passwd, err := readRegular(passwdPath)
	if err != nil {
		r.m.addWarning("uid remap: /etc/passwd: %v", err)
		return nil
	}
	group, err := readRegular(groupPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.m.addWarning("uid remap: /etc/group: %v", err)
		return nil
	}
	newPasswd, newGroup, res := RemapUser(passwd, group, rm.User, rm.UID, rm.GID)
	r.m.Remap = &res
	r.logf("uid remap of %s: %s", rm.User, remapSummary(res))
	if !res.Changed {
		return nil
	}
	if err := replaceKeepingMeta(passwdPath, newPasswd); err != nil {
		return fmt.Errorf("uid remap: write /etc/passwd: %w", err)
	}
	if group != nil && string(newGroup) != string(group) {
		if err := replaceKeepingMeta(groupPath, newGroup); err != nil {
			return fmt.Errorf("uid remap: write /etc/group: %w", err)
		}
	}
	if res.Home == "" {
		return nil
	}
	home, err := g.resolveDir(res.Home)
	if err != nil {
		r.m.addWarning("uid remap: home %s: %v", res.Home, err)
		return nil
	}
	return chownTree(home, res.UID, res.GID)
}

func remapSummary(res RemapResult) string {
	if !res.Changed {
		return res.Note
	}
	s := fmt.Sprintf("%d:%d -> %d:%d", res.FromUID, res.FromGID, res.UID, res.GID)
	if res.Note != "" {
		s += " (" + res.Note + ")"
	}
	return s
}

// installFleet installs this binary into fleet/bin and links /usr/bin/fleet in
// the new root to it.
func (r *seedRun) installFleet(g genRoot) error {
	r.setPhase("fleet binary")
	sha, err := FileSHA256(selfExe)
	if err != nil {
		return fmt.Errorf("hash this binary: %w", err)
	}
	if _, err := installBinary(selfExe, sha, filepath.Join(r.pvc, FleetBinDir)); err != nil {
		return fmt.Errorf("install the fleet binary: %w", err)
	}
	r.m.Binary = sha
	linked, err := linkFleet(g)
	if err != nil {
		return fmt.Errorf("link %s: %w", FleetLink, err)
	}
	if !linked {
		r.m.addWarning("the image has no /usr/bin: %s not linked", FleetLink)
	}
	return nil
}

// receiveWorkspace takes workspace transfers until one extracts cleanly.
func (r *seedRun) receiveWorkspace(ctx context.Context) (WorkspaceStats, error) {
	dst := filepath.Join(r.pvc, DirWorkspace)
	for {
		ws, err := r.srv.Workspace(ctx, r.stall)
		if err != nil {
			return WorkspaceStats{}, err
		}
		st, err := ExtractWorkspace(ws, dst, r.seed.Workspace.Owner, func(st WorkspaceStats) {
			r.srv.Progress(jobboot.Progress{Phase: "workspace", Bytes: st.Bytes, Files: st.Files})
		})
		if err == nil {
			ws.Accept(st.Files, st.Bytes)
			r.logf("workspace: %d files, %d bytes", st.Files, st.Bytes)
			return st, nil
		}
		if errors.Is(err, jobboot.ErrStalled) || ctx.Err() != nil {
			_ = ws.Close()
			return st, err
		}
		r.logf("workspace transfer failed, waiting for another: %v", err)
		ws.Reject(err)
	}
}

// mkdirOwned creates dir (and parents) with mode, owned by the job's own uid,
// and leaves an existing one alone.
func mkdirOwned(dir string, mode os.FileMode) error {
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// writePlaceholder replaces whatever is at p (a symlink included, never
// followed) with a regular 0644 root-owned file holding data.
func writePlaceholder(p string, data []byte) error {
	if fi, err := os.Lstat(p); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", p)
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && os.Geteuid() == 0 {
		err = f.Chown(0, 0)
	}
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readRegular reads p, which must be a regular file.
func readRegular(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return os.ReadFile(p)
}

// replaceKeepingMeta rewrites the regular file p with data through a temp file
// and a rename, keeping its mode and owner (as sed -i leaves it).
func replaceKeepingMeta(p string, data []byte) error {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := unix.Fchown(int(tmp.Fd()), int(st.Uid), int(st.Gid)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := unix.Fchmod(int(tmp.Fd()), st.Mode&0o7777); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// chownTree is `chown -R uid:gid dir`: every entry, symlinks themselves (never
// followed), as the CLI's uid layer chowns the home dir.
func chownTree(dir string, uid, gid int) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// syncFS flushes the filesystem holding dir.
func syncFS(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

func setOf(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range slices.Clone(items) {
		m[s] = true
	}
	return m
}
