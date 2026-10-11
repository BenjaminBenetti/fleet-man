//go:build linux

package podseed

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

// jobEnv is a seed pod faked on the local filesystem: the image root, the PVC
// and tools mounts, the kubelet's resolv.conf and termination log, and the
// staged binary.
type jobEnv struct {
	img                 *image
	pvc, tools, termLog string
	binary, binarySHA   string
}

func newJobEnv(t *testing.T) *jobEnv {
	t.Helper()
	e := &jobEnv{img: makeImage(t), pvc: filepath.Join(t.TempDir(), "pvc")}
	mustMkdir(t, e.pvc, 0o755)
	t.Cleanup(func() { _ = removeTree(e.pvc) })
	tools, err := os.MkdirTemp("/tmp", "pj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tools) })
	e.tools = tools
	e.termLog = filepath.Join(t.TempDir(), "termination-log")

	e.binary = filepath.Join(t.TempDir(), "fleet")
	content := []byte("#!/bin/sh\n# a stand-in fleet-pod binary\n")
	if err := os.WriteFile(e.binary, content, 0o755); err != nil {
		t.Fatal(err)
	}
	e.binarySHA = sha256Hex(content)
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("search ns.svc.cluster.local\nnameserver 10.96.0.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldExe, oldResolv := selfExe, liveResolvConf
	selfExe, liveResolvConf = e.binary, resolv
	t.Cleanup(func() { selfExe, liveResolvConf = oldExe, oldResolv })
	// The kubelet's /etc/hosts, the PVC and the tools dir are the seed pod's
	// mounts.
	fakeMountInfo(t, filepath.Join(e.img.root, "etc/hosts"))
	return e
}

func (e *jobEnv) spec(op Op, gen int) JobSpec {
	return JobSpec{V: JobSpecVersion, Op: op, Gen: gen, BinarySHA256: e.binarySHA, PVC: e.pvc,
		Tools: e.tools, TerminationLog: e.termLog, StallTimeout: "20s", ResultHold: "10s"}
}

func (e *jobEnv) seedSpec(gen int) JobSpec {
	s := e.spec(OpSeed, gen)
	s.Seed = &SeedSpec{
		Source:    e.img.root,
		MountDirs: []string{"/workspaces/fleet-man", "/var/lib/docker", "/opt/fleet", "/fleet-mounts/control"},
		Hostname:  "fi-fleet-man-alpha-1a2b3c4d-0",
		Remap:     &Remap{User: "vscode", UID: 4321, GID: 4321},
		Volumes: []Volume{
			{Name: "docker", Target: "/var/lib/docker", CopyUp: true},
			{Name: "cache", Target: "/home/vscode/.cache"},
		},
		Workers: 4,
	}
	return s
}

// runJob runs pod-job on spec and, when feed is set, feeds it the way fleetd
// does, returning the exit code, the collected result and the job's log.
func (e *jobEnv) runJob(t *testing.T, spec JobSpec, feed bool, workspace []byte) (int, jobboot.Result, string) {
	t.Helper()
	data, _ := json.Marshal(spec)
	specFile := filepath.Join(e.tools, jobboot.SpecName)
	if err := os.WriteFile(specFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- RunJob(context.Background(), specFile, &log) }()
	var res jobboot.Result
	if feed {
		conn := dialJob(t, jobboot.Paths{Dir: e.tools}.Socket())
		defer conn.Close()
		opts := jobboot.FeedOptions{}
		if workspace != nil {
			opts.Workspace = bytes.NewReader(workspace)
		}
		var err error
		res, err = jobboot.Feed(context.Background(), conn, opts)
		if err != nil {
			t.Fatalf("feed: %v\n%s", err, log.String())
		}
	}
	select {
	case c := <-code:
		return c, res, log.String()
	case <-time.After(60 * time.Second):
		t.Fatalf("pod-job did not exit\n%s", log.String())
	}
	return 0, res, ""
}

func dialJob(t *testing.T, sock string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", sock, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *jobEnv) termination(t *testing.T) jobboot.Termination {
	t.Helper()
	data, err := os.ReadFile(e.termLog)
	if err != nil {
		t.Fatal(err)
	}
	term, err := jobboot.ParseTermination(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return term
}

func workspaceTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: fixtureTime})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: fixtureTime})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestSeedJobEndToEnd(t *testing.T) {
	e := newJobEnv(t)
	// Content for volumes copied up from inside remoteUser's home, and from
	// a dir holding it.
	mustMkdir(t, filepath.Join(e.img.root, "home/vscode/.config"), 0o755)
	mustMkdir(t, filepath.Join(e.img.root, "home/other"), 0o755)
	for _, f := range []string{"home/vscode/.config/app.conf", "home/other/notes"} {
		if err := os.WriteFile(filepath.Join(e.img.root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if os.Geteuid() == 0 {
		mustChown(t, filepath.Join(e.img.root, "home/vscode/.config"), 1000, 1000)
		mustChown(t, filepath.Join(e.img.root, "home/vscode/.config/app.conf"), 1000, 1000)
		mustChown(t, filepath.Join(e.img.root, "home/other/notes"), 2000, 2000)
	}
	spec := e.seedSpec(1)
	spec.Seed.Workspace = &Workspace{Owner: &IDs{UID: 4321, GID: 4321}}
	spec.Seed.Volumes = append(spec.Seed.Volumes,
		Volume{Name: "config", Target: "/home/vscode/.config", CopyUp: true},
		Volume{Name: "homes", Target: "/home", CopyUp: true})
	code, res, log := e.runJob(t, spec, true, workspaceTar(t, map[string]string{"README.md": "# fleet-man\n"}))
	if code != 0 || !res.OK {
		t.Fatalf("exit %d, result %+v\n%s", code, res, log)
	}
	m, err := ParseManifest(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"etc", "home", "opt", "run", "srv", "tmp", "usr", "var"}; !reflect.DeepEqual(m.Dirs, want) {
		t.Errorf("dirs %v, want %v", m.Dirs, want)
	}
	if want := []string{"fleet-mounts", "workspaces"}; !reflect.DeepEqual(m.RootExtra, want) {
		t.Errorf("root-extra %v, want %v", m.RootExtra, want)
	}
	if m.Gen != 1 || m.Binary != e.binarySHA || m.Files == 0 || m.Bytes == 0 || m.Setuid != 1 {
		t.Errorf("manifest %+v", m)
	}
	if want := []string{"docker", "cache", "config", "homes"}; !reflect.DeepEqual(m.Volumes, want) {
		t.Errorf("volumes %v", m.Volumes)
	}
	onDisk, err := os.ReadFile(filepath.Join(e.pvc, ManifestFile))
	if err != nil || !bytes.Equal(onDisk, res.Output) {
		t.Fatalf("the manifest on the PVC differs from the result: %v", err)
	}
	term := e.termination(t)
	if !term.OK || term.SHA256 != sha256Hex(onDisk) || term.Bytes != res.Bytes || term.Bytes == 0 {
		t.Errorf("termination %+v (manifest sha %s)", term, sha256Hex(onDisk))
	}

	read := func(p string) string {
		data, err := os.ReadFile(filepath.Join(e.pvc, p))
		if err != nil {
			t.Errorf("read %s: %v", p, err)
		}
		return string(data)
	}
	if read("ws/README.md") != "# fleet-man\n" {
		t.Error("the workspace was not extracted")
	}
	if read(ResolvConfFile) != "search ns.svc.cluster.local\nnameserver 10.96.0.10\n" || read(HostnameFile) != "fi-fleet-man-alpha-1a2b3c4d-0\n" {
		t.Error("meta/resolv.conf or meta/hostname")
	}
	for _, f := range kubeletFiles {
		fi, err := os.Lstat(filepath.Join(e.pvc, "root/etc", f))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 || fi.Mode().Perm() != 0o644 {
			t.Errorf("placeholder /etc/%s: %v %v", f, fi, err)
		}
	}
	if target, _ := os.Readlink(filepath.Join(e.pvc, "root/usr/bin/fleet")); target != CurrentBinary {
		t.Errorf("/usr/bin/fleet -> %q", target)
	}
	if target, _ := os.Readlink(filepath.Join(e.pvc, FleetBinDir, "fleet")); target != BinaryName(e.binarySHA) {
		t.Errorf("fleet/bin/fleet -> %q", target)
	}
	if fi, err := os.Stat(filepath.Join(e.pvc, FleetLogDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("fleet/log: %v %v, want a private dir", fi, err)
	}
	for _, dir := range []string{"root-extra/workspaces", "root-extra/fleet-mounts", "fleet/log", "fleet/spec", "fleet/run", "vol/cache"} {
		if fi, err := os.Stat(filepath.Join(e.pvc, dir)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if read("vol/docker/image-content") != "under a volume target" {
		t.Error("the docker volume was not copied up")
	}
	if _, err := os.Lstat(filepath.Join(e.pvc, "root/var/lib/docker/image-content")); !os.IsNotExist(err) {
		t.Error("content under the volume target was copied into the root too")
	}
	if _, err := os.Lstat(filepath.Join(e.pvc, "root/proc")); !os.IsNotExist(err) {
		t.Error("/proc was copied")
	}

	passwd := read("root/etc/passwd")
	ws := lstat(t, filepath.Join(e.pvc, "ws"))
	home := lstat(t, filepath.Join(e.pvc, "root/home/vscode/.bashrc"))
	if os.Geteuid() == 0 {
		if !strings.Contains(passwd, "vscode:x:4321:4321:") || !strings.Contains(read("root/etc/group"), "vscode:x:4321:") {
			t.Errorf("remap not applied:\n%s", passwd)
		}
		if m.Remap == nil || !m.Remap.Changed || home.Uid != 4321 || ws.Uid != 4321 {
			t.Errorf("remap %+v, home owner %d, ws owner %d", m.Remap, home.Uid, ws.Uid)
		}
		// Volumes are filled from the image as the CLI's uid layer left it:
		// remoteUser's home remapped, the rest as it was.
		for p, want := range map[string]uint32{
			"vol/config": 4321, "vol/config/app.conf": 4321,
			"vol/homes/vscode": 4321, "vol/homes/vscode/.bashrc": 4321,
			"vol/homes/other/notes": 2000, "vol/docker/image-content": 0,
		} {
			if st := lstat(t, filepath.Join(e.pvc, p)); st.Uid != want {
				t.Errorf("%s: owner %d, want %d", p, st.Uid, want)
			}
		}
		if st := lstat(t, filepath.Join(e.pvc, "root/etc/passwd")); st.Mode&0o777 != 0o644 || st.Uid != 0 {
			t.Errorf("rewritten passwd: mode %o owner %d", st.Mode, st.Uid)
		}
	} else {
		if !m.SingleUID || !strings.Contains(strings.Join(m.Warnings, "\n"), "no uid remap without root") {
			t.Errorf("a non-root seed: single_uid %v warnings %v", m.SingleUID, m.Warnings)
		}
		if passwd != imagePasswd {
			t.Error("a non-root seed rewrote passwd")
		}
	}
}

func TestReseedRollbackAndCommitJobs(t *testing.T) {
	e := newJobEnv(t)
	if code, res, log := e.runJob(t, e.seedSpec(1), true, nil); code != 0 || !res.OK {
		t.Fatalf("gen 1: %d %+v\n%s", code, res, log)
	}
	// The user's changes to generation 1, a kept volume's data, and the
	// workspace a rebuild keeps.
	if err := os.WriteFile(filepath.Join(e.pvc, "root/etc/user-edit"), []byte("gen1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(e.pvc, "root-extra/workspaces/sibling"), 0o755)
	if err := os.WriteFile(filepath.Join(e.pvc, "vol/docker/data"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.pvc, "ws/WIP"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The rebuilt image has a new file.
	if err := os.WriteFile(filepath.Join(e.img.root, "usr/bin/new-tool"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	reseed := e.seedSpec(2)
	reseed.Seed.Reset, reseed.Seed.PrevGen = true, 1
	code, res, log := e.runJob(t, reseed, true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("gen 2: %d %+v\n%s", code, res, log)
	}
	if m, _ := ParseManifest(res.Output); m.Gen != 2 || len(m.Volumes) != 0 {
		t.Fatalf("gen 2 manifest %+v (kept volumes are not seeded again)", m)
	}
	exists := func(p string) bool { _, err := os.Lstat(filepath.Join(e.pvc, p)); return err == nil }
	if exists("root/etc/user-edit") || !exists("root/usr/bin/new-tool") || exists("root-extra/workspaces/sibling") {
		t.Fatal("generation 2 is not a fresh copy of the new image")
	}
	if !exists("root.g1.trash/etc/user-edit") || !exists("root-extra.g1.trash/workspaces/sibling") {
		t.Fatal("generation 1 is not in trash")
	}
	if !exists("vol/docker/data") || !exists("ws/WIP") {
		t.Fatal("the reseed lost a volume or the workspace")
	}

	// The rebuild failed later: roll back.
	code, res, log = e.runJob(t, e.spec(OpRollback, 1), true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("rollback: %d %+v\n%s", code, res, log)
	}
	if m, _ := ParseManifest(res.Output); m.Gen != 1 {
		t.Fatalf("after the rollback the PVC holds generation %d", m.Gen)
	}
	if !exists("root/etc/user-edit") || exists("root/usr/bin/new-tool") || !exists("root-extra/workspaces/sibling") || exists("root.g1.trash") {
		t.Fatal("the rollback did not restore generation 1")
	}

	// Again, and this time it sticks.
	if code, res, log = e.runJob(t, reseed, true, nil); code != 0 || !res.OK {
		t.Fatalf("gen 2 again: %d %+v\n%s", code, res, log)
	}
	if code, res, log = e.runJob(t, e.spec(OpCommit, 2), true, nil); code != 0 || !res.OK {
		t.Fatalf("commit: %d %+v\n%s", code, res, log)
	}
	if exists("root.g1.trash") || exists("root-extra.g1.trash") || exists(trashManifest(1)) || !exists("root/usr/bin/new-tool") {
		t.Fatal("the commit left trash or lost generation 2")
	}
}

func TestSeedJobRefusesToSeedOverAnotherGeneration(t *testing.T) {
	e := newJobEnv(t)
	if code, _, log := e.runJob(t, e.seedSpec(1), true, nil); code != 0 {
		t.Fatalf("gen 1: %d\n%s", code, log)
	}
	code, res, _ := e.runJob(t, e.seedSpec(2), true, nil)
	if code != jobboot.ExitFailed || res.OK || !strings.Contains(res.Error, "needs a reset") {
		t.Fatalf("a non-reset seed over generation 1: exit %d %+v", code, res)
	}
	// A retry of the same generation redoes it.
	if code, res, log := e.runJob(t, e.seedSpec(1), true, nil); code != 0 || !res.OK {
		t.Fatalf("a retried gen 1: %d %+v\n%s", code, res, log)
	}
}

func TestSeedJobStallsWithoutFleetd(t *testing.T) {
	e := newJobEnv(t)
	spec := e.seedSpec(1)
	spec.StallTimeout, spec.ResultHold = "400ms", "100ms"
	spec.Seed.Workspace = &Workspace{}
	code, _, log := e.runJob(t, spec, false, nil)
	if code != jobboot.ExitStalled {
		t.Fatalf("exit %d, want %d\n%s", code, jobboot.ExitStalled, log)
	}
	if term := e.termination(t); term.OK || !strings.Contains(term.Error, "stall") {
		t.Fatalf("termination %+v", term)
	}
}

func TestPodJobRefusesAnotherBinaryOrABadSpec(t *testing.T) {
	e := newJobEnv(t)
	spec := e.seedSpec(1)
	spec.BinarySHA256 = strings.Repeat("0", 64)
	if code, _, _ := e.runJob(t, spec, false, nil); code != jobboot.ExitBadSpec {
		t.Fatalf("another binary: exit %d", code)
	}
	if term := e.termination(t); term.OK || !strings.Contains(term.Error, "sha256") {
		t.Fatalf("termination %+v", term)
	}
	spec = e.seedSpec(1)
	spec.Gen = 0
	if code, _, _ := e.runJob(t, spec, false, nil); code != jobboot.ExitBadSpec {
		t.Fatalf("a bad spec: exit %d", code)
	}
}

func TestSeedJobCrashMidResetIsFinishedByARerun(t *testing.T) {
	e := newJobEnv(t)
	if code, _, log := e.runJob(t, e.seedSpec(1), true, nil); code != 0 {
		t.Fatalf("gen 1: %d\n%s", code, log)
	}
	reseed := e.seedSpec(2)
	reseed.Seed.Reset, reseed.Seed.PrevGen = true, 1
	injectAt(t, "reset:root-extra")
	if code, res, _ := e.runJob(t, reseed, true, nil); code != jobboot.ExitFailed || !strings.Contains(res.Error, "injected") {
		t.Fatalf("the crashed reseed: exit %d %+v", code, res)
	}
	if code, res, log := e.runJob(t, reseed, true, nil); code != 0 || !res.OK {
		t.Fatalf("the rerun: %d %+v\n%s", code, res, log)
	}
	if m, err := ReadManifest(filepath.Join(e.pvc, ManifestFile)); err != nil || m.Gen != 2 {
		t.Fatalf("after the rerun: %+v %v", m, err)
	}
}

func TestSeedJobFallsBackToCpForADirTheCopierFails(t *testing.T) {
	if _, ok := gnuCP(); !ok {
		t.Skip("no GNU cp here")
	}
	e := newJobEnv(t)
	injectAt(t, "copy:/usr/bin/tool")
	code, res, log := e.runJob(t, e.seedSpec(1), true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("exit %d %+v\n%s", code, res, log)
	}
	m, _ := ParseManifest(res.Output)
	if !reflect.DeepEqual(m.Fallback, []string{"usr"}) || !strings.Contains(log, "copying it again with cp") {
		t.Fatalf("fallback %v\n%s", m.Fallback, log)
	}
	for _, p := range []string{"usr/bin/tool", "usr/lib/libx.so"} {
		a, _ := os.ReadFile(filepath.Join(e.img.root, p))
		b, err := os.ReadFile(filepath.Join(e.pvc, "root", p))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s after cp: %v", p, err)
		}
	}
	if st := lstat(t, filepath.Join(e.pvc, "root/usr/bin/sudo")); st.Mode&0o7777 != 0o4755 {
		t.Errorf("cp lost the setuid bit: %o", st.Mode)
	}
	if target, _ := os.Readlink(filepath.Join(e.pvc, "root/usr/bin/fleet")); target != CurrentBinary {
		t.Errorf("/usr/bin/fleet -> %q after the fallback", target)
	}
	// The xattrs cp is not given still arrive.
	if want, err := getXattr(filepath.Join(e.img.root, "usr/bin/tool"), -1, "user.fleet.test"); err == nil {
		if got, err := getXattr(filepath.Join(e.pvc, "root/usr/bin/tool"), -1, "user.fleet.test"); err != nil || !bytes.Equal(got, want) {
			t.Errorf("user xattr after cp: %q %v", got, err)
		}
	}
	if e.img.caps {
		if got, err := getXattr(filepath.Join(e.pvc, "root/usr/bin/ping"), -1, capabilityXattr); err != nil || !bytes.Equal(got, pingCaps()) {
			t.Errorf("file capability after cp: %x %v", got, err)
		}
	}
}

func TestCPFallbackKeepsXattrsButNotTheImagesSELinuxLabel(t *testing.T) {
	requireRoot(t)
	if _, ok := gnuCP(); !ok {
		t.Skip("no GNU cp here")
	}
	e := newJobEnv(t)
	injectAt(t, "copy:/etc/os-release")
	code, res, log := e.runJob(t, e.seedSpec(1), true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("exit %d %+v\n%s", code, res, log)
	}
	if m, _ := ParseManifest(res.Output); !reflect.DeepEqual(m.Fallback, []string{"etc"}) {
		t.Fatalf("fallback %v", m.Fallback)
	}
	src, dst := filepath.Join(e.img.root, "etc/os-release"), filepath.Join(e.pvc, "root/etc/os-release")
	if e.img.selinux {
		// The node labels the new file where it has SELinux; the image's own
		// label must never arrive.
		want, _ := getXattr(src, -1, selinuxXattr)
		if got, err := getXattr(dst, -1, selinuxXattr); err == nil && bytes.Equal(got, want) {
			t.Errorf("cp carried security.selinux %q over", got)
		}
	} else {
		t.Log("security.selinux cannot be set here; unchecked")
	}
	if e.img.acl {
		if got, err := getXattr(dst, -1, "system.posix_acl_access"); err != nil || !bytes.Equal(got, userACL()) {
			t.Errorf("ACL after cp: %x %v", got, err)
		}
	}
	if e.img.trusted {
		if got, err := getXattr(dst, -1, "trusted.fleet"); err != nil || string(got) != "t" {
			t.Errorf("trusted xattr after cp: %q %v", got, err)
		}
	}
}

// A tool Job reports the disk's space and holds its pod until fleetd
// collects that: the workspace export ran beside it meanwhile.
func TestToolJobReportsTheDisk(t *testing.T) {
	e := newJobEnv(t)
	if code, res, log := e.runJob(t, e.seedSpec(1), true, nil); code != 0 || !res.OK {
		t.Fatalf("gen 1: %d %+v\n%s", code, res, log)
	}
	code, res, log := e.runJob(t, e.spec(OpTool, 1), true, nil)
	if code != 0 || !res.OK {
		t.Fatalf("tool: %d %+v\n%s", code, res, log)
	}
	var tr ToolResult
	if err := json.Unmarshal(res.Output, &tr); err != nil {
		t.Fatalf("tool output %s: %v", res.Output, err)
	}
	if tr.Disk.Total <= 0 || tr.Disk.Free <= 0 || tr.Disk.Free > tr.Disk.Total {
		t.Fatalf("tool disk %+v", tr.Disk)
	}
	if term := e.termination(t); !term.OK {
		t.Fatalf("termination %+v", term)
	}
}
