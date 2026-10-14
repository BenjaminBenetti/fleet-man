//go:build linux

package podseed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"golang.org/x/sys/unix"
)

// seededSource seeds a source instance disk the way a create does, and leaves
// what a running instance adds: PID 1 logs, share-sync state, a trashed
// generation, a volume's data, a workspace edit.
func seededSource(t *testing.T, e *jobEnv) string {
	t.Helper()
	spec := e.seedSpec(1)
	spec.Seed.Workspace = &Workspace{}
	code, res, log := e.runJob(t, spec, true, workspaceTar(t, map[string]string{"README.md": "# fleet-man\n"}))
	if code != 0 || !res.OK {
		t.Fatalf("seed: exit %d, %+v\n%s", code, res, log)
	}
	src := e.pvc
	for p, body := range map[string]string{
		"fleet/log/pid1.log":          "the source's PID 1 output",
		"sync/state.json":             "{}",
		"vol/docker/pulled-image":     "the source's docker data",
		"ws/uncommitted.txt":          "work in progress",
		"root/home/vscode/.history":   "ls\n",
		"meta/rootfs.g1.json":         "{}",
		"root.g1.trash/usr/bin/stale": "x",
	} {
		mustMkdir(t, filepath.Dir(filepath.Join(src, p)), 0o755)
		if err := os.WriteFile(filepath.Join(src, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// cloneEnv is a second disk in e's pod, the clone's, for a clone of src.
func cloneEnv(t *testing.T, e *jobEnv) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone-pvc")
	mustMkdir(t, dst, 0o755)
	t.Cleanup(func() { _ = removeTree(dst) })
	old := cloneImageRoot
	cloneImageRoot = e.img.root
	t.Cleanup(func() { cloneImageRoot = old })
	return dst
}

func (e *jobEnv) cloneSpec(dst, src string, live bool) JobSpec {
	s := e.spec(OpClone, 1)
	s.PVC = dst
	s.Clone = &CloneSpec{
		Source: src, Live: live, Hostname: "fi-fleet-man-beta-5e6f7g8h-0",
		MountDirs: []string{"/workspaces/fleet-man", "/var/lib/docker", "/opt/fleet", "/fleet-mounts/control"},
		Volumes: []Volume{
			{Name: "docker", Target: "/var/lib/docker", CopyUp: true},
			{Name: "cache", Target: "/home/vscode/.cache"},
		},
		Workers: 4,
	}
	return s
}

// runClone runs the clone job, feeding it with barrier as fleetd's barrier
// handler.
func (e *jobEnv) runClone(t *testing.T, spec JobSpec, barrier func(context.Context, jobboot.Barrier) error) (int, jobboot.Result, string) {
	t.Helper()
	data, _ := json.Marshal(spec)
	specFile := filepath.Join(e.tools, jobboot.SpecName)
	if err := os.WriteFile(specFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- RunJob(context.Background(), specFile, &log) }()
	conn := dialJob(t, jobboot.Paths{Dir: e.tools}.Socket())
	defer conn.Close()
	res, err := jobboot.Feed(context.Background(), conn, jobboot.FeedOptions{Barrier: barrier})
	if err != nil {
		t.Fatalf("feed: %v\n%s", err, log.String())
	}
	select {
	case c := <-code:
		return c, res, log.String()
	case <-time.After(60 * time.Second):
		t.Fatalf("pod-job did not exit\n%s", log.String())
	}
	return 0, res, ""
}

func readIn(t *testing.T, root, p string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Errorf("read %s: %v", p, err)
	}
	return string(data)
}

// checkCloneFixUps checks what a clone's disk has of its own, whichever tier
// made it.
func checkCloneFixUps(t *testing.T, dst string, res jobboot.Result) Manifest {
	t.Helper()
	m, err := ParseManifest(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	if m.Gen != 1 || !reflect.DeepEqual(m.Volumes, []string{"docker", "cache"}) {
		t.Errorf("manifest gen %d volumes %v", m.Gen, m.Volumes)
	}
	if got := readIn(t, dst, HostnameFile); got != "fi-fleet-man-beta-5e6f7g8h-0\n" {
		t.Errorf("hostname %q", got)
	}
	if got := readIn(t, dst, "vol/docker/image-content"); got != "under a volume target" {
		t.Errorf("the docker volume is not the image's copy-up: %q", got)
	}
	for _, gone := range []string{"vol/docker/pulled-image", "fleet/log/pid1.log", "sync", "meta/rootfs.g1.json", "root.g1.trash"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !os.IsNotExist(err) {
			t.Errorf("the clone has the source's %s (%v)", gone, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, FleetLogDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("fleet/log: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "vol/cache")); err != nil || !fi.IsDir() {
		t.Errorf("vol/cache: %v", err)
	}
	return m
}

func TestCloneCopiesAStoppedSourcesDisk(t *testing.T) {
	e := newJobEnv(t)
	src := seededSource(t, e)
	dst := cloneEnv(t, e)
	var barriers []string
	code, res, log := e.runClone(t, e.cloneSpec(dst, src, false), func(_ context.Context, b jobboot.Barrier) error {
		barriers = append(barriers, b.Name)
		return nil
	})
	if code != 0 || !res.OK {
		t.Fatalf("exit %d, %+v\n%s", code, res, log)
	}
	if len(barriers) != 0 {
		t.Errorf("a stopped source's copy stopped at %v", barriers)
	}
	checkCloneFixUps(t, dst, res)
	for _, p := range []string{"ws/README.md", "ws/uncommitted.txt", "root/home/vscode/.history", "root/etc/passwd", "root/usr/bin/tool"} {
		if readIn(t, dst, p) != readIn(t, src, p) {
			t.Errorf("%s differs from the source's", p)
		}
	}
	// What the copier keeps of a file: mode, times, hard links, holes.
	sst, dstSt := lstat(t, filepath.Join(src, "root/usr/bin/sudo")), lstat(t, filepath.Join(dst, "root/usr/bin/sudo"))
	if sst.Mode != dstSt.Mode || sst.Mtim != dstSt.Mtim {
		t.Errorf("sudo: mode %o mtime %v, want %o %v", dstSt.Mode, dstSt.Mtim, sst.Mode, sst.Mtim)
	}
	a, b := lstat(t, filepath.Join(dst, "root/usr/lib/libx.so")), lstat(t, filepath.Join(dst, "root/usr/lib/libx.so.1"))
	if a.Ino != b.Ino {
		t.Error("the hard link became two files")
	}
	if sp := lstat(t, filepath.Join(dst, "root/var/sparse.img")); sp.Blocks*512 >= sp.Size {
		t.Errorf("the sparse file lost its holes: %d blocks for %d bytes", sp.Blocks, sp.Size)
	}
	if target, _ := os.Readlink(filepath.Join(dst, "root/usr/bin/fleet")); target != CurrentBinary {
		t.Errorf("/usr/bin/fleet -> %q", target)
	}
	term := e.termination(t)
	if !term.OK || term.SHA256 != sha256Hex(res.Output) {
		t.Errorf("termination %+v", term)
	}
}

func TestALiveCloneTakesTheFinalDeltaBetweenItsBarriers(t *testing.T) {
	e := newJobEnv(t)
	src := seededSource(t, e)
	dst := cloneEnv(t, e)
	var (
		mu       sync.Mutex
		barriers []string
	)
	// While fleetd holds the source at the final barrier, the source's last
	// writes land: a new file, an edit that keeps its size and modification
	// time, a delete. The final delta must carry each.
	edited := filepath.Join(src, "ws/README.md")
	before := lstat(t, edited)
	code, res, log := e.runClone(t, e.cloneSpec(dst, src, true), func(_ context.Context, b jobboot.Barrier) error {
		mu.Lock()
		barriers = append(barriers, b.Name)
		mu.Unlock()
		if b.Name != BarrierFinal {
			return nil
		}
		if err := os.WriteFile(filepath.Join(src, "ws/late.txt"), []byte("written at the end"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(edited, []byte("# fleet-mon\n"), 0o644); err != nil {
			return err
		}
		ts := []unix.Timespec{before.Atim, before.Mtim}
		if err := unix.UtimesNano(edited, ts); err != nil {
			return err
		}
		return os.Remove(filepath.Join(src, "ws/uncommitted.txt"))
	})
	if code != 0 || !res.OK {
		t.Fatalf("exit %d, %+v\n%s", code, res, log)
	}
	if !reflect.DeepEqual(barriers, []string{BarrierFinal, BarrierCopied}) {
		t.Fatalf("barriers %v", barriers)
	}
	checkCloneFixUps(t, dst, res)
	if got := readIn(t, dst, "ws/late.txt"); got != "written at the end" {
		t.Errorf("the final delta missed a new file: %q", got)
	}
	if got := readIn(t, dst, "ws/README.md"); got != "# fleet-mon\n" {
		t.Errorf("the final delta missed an edit that kept size and mtime: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dst, "ws/uncommitted.txt")); !os.IsNotExist(err) {
		t.Errorf("the final delta missed a delete: %v", err)
	}
}

func TestALiveCloneFailsWhenFleetdRefusesTheFinalDelta(t *testing.T) {
	e := newJobEnv(t)
	src := seededSource(t, e)
	dst := cloneEnv(t, e)
	var barriers []string
	code, res, _ := e.runClone(t, e.cloneSpec(dst, src, true), func(_ context.Context, b jobboot.Barrier) error {
		barriers = append(barriers, b.Name)
		return os.ErrDeadlineExceeded
	})
	if code == 0 || res.OK || !strings.Contains(res.Error, "final delta") {
		t.Fatalf("exit %d, %+v", code, res)
	}
	if !reflect.DeepEqual(barriers, []string{BarrierFinal}) {
		t.Fatalf("barriers %v", barriers)
	}
}

// TestCloneFixesUpARestoredDisk: a snapshot or CSI clone restored the source's
// disk onto the clone's; the job makes it the clone's.
func TestCloneFixesUpARestoredDisk(t *testing.T) {
	e := newJobEnv(t)
	src := seededSource(t, e)
	dst := cloneEnv(t, e)
	if out, err := exec.Command("cp", "-a", src+"/.", dst+"/").CombinedOutput(); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	code, res, log := e.runClone(t, e.cloneSpec(dst, "", false), nil)
	if code != 0 || !res.OK {
		t.Fatalf("exit %d, %+v\n%s", code, res, log)
	}
	checkCloneFixUps(t, dst, res)
	if readIn(t, dst, "ws/uncommitted.txt") != "work in progress" {
		t.Error("the restored workspace was touched")
	}
}

func TestCloneOfAnEmptyDiskFails(t *testing.T) {
	e := newJobEnv(t)
	dst := cloneEnv(t, e)
	code, res, _ := e.runClone(t, e.cloneSpec(dst, "", false), nil)
	if code == 0 || res.OK || !strings.Contains(res.Error, ErrNoInstance.Error()) {
		t.Fatalf("a restore that came up empty: exit %d, %+v", code, res)
	}
}

func TestCloneRefusesASourceOfAnotherGeneration(t *testing.T) {
	e := newJobEnv(t)
	src := seededSource(t, e)
	dst := cloneEnv(t, e)
	spec := e.cloneSpec(dst, src, false)
	spec.Gen = 2
	code, res, _ := e.runClone(t, spec, nil)
	if code == 0 || res.OK || !strings.Contains(res.Error, "rebuilt meanwhile") {
		t.Fatalf("exit %d, %+v", code, res)
	}
}

func TestCloneSpecValidates(t *testing.T) {
	ok := JobSpec{V: JobSpecVersion, Op: OpClone, Gen: 1, PVC: "/fleet-pvc", Clone: &CloneSpec{Source: "/fleet-src", Live: true, Hostname: "h-0"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*JobSpec){
		"no clone spec":    func(s *JobSpec) { s.Clone = nil },
		"a seed spec too":  func(s *JobSpec) { s.Seed = &SeedSpec{Hostname: "h"} },
		"live, no source":  func(s *JobSpec) { s.Clone.Source = "" },
		"relative source":  func(s *JobSpec) { s.Clone.Source = "fleet-src" },
		"no hostname":      func(s *JobSpec) { s.Clone.Hostname = "" },
		"negative passes":  func(s *JobSpec) { s.Clone.Passes = -1 },
		"bad volume":       func(s *JobSpec) { s.Clone.Volumes = []Volume{{Name: "../x", Target: "/x"}} },
		"relative mount":   func(s *JobSpec) { s.Clone.MountDirs = []string{"x"} },
		"rollback + clone": func(s *JobSpec) { s.Op = OpRollback },
	} {
		s := ok
		c := *ok.Clone
		s.Clone = &c
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}
