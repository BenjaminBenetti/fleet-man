package podinit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

func validSpec() Spec {
	return Spec{
		V:          SpecVersion,
		Argv:       []string{"/bin/sh", "-c", "echo Container started\ntrap \"exit 0\" 15\nexec \"$@\"\nwhile sleep 1 & wait $!; do :; done", "-"},
		User:       "vscode",
		WorkingDir: "/workspaces/fleet-man",
		Env:        map[string]string{"LABEL_ONLY": "x"},
		StopSignal: "SIGINT",
		Rlimits:    []Rlimit{{Name: "nofile", Soft: 1024, Hard: 4096}, {Name: "core", Soft: -1, Hard: -1}},
		Hostname:   "devbox",
		DockerMTU:  &DockerMTU{},
		RemoteUser: "vscode",
	}
}

func TestSpecValidate(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Spec){
		"version":      func(s *Spec) { s.V = 2 },
		"no argv":      func(s *Spec) { s.Argv = nil },
		"empty argv0":  func(s *Spec) { s.Argv = []string{""} },
		"nul":          func(s *Spec) { s.Argv = []string{"a\x00b"} },
		"workdir":      func(s *Spec) { s.WorkingDir = "relative" },
		"env name":     func(s *Spec) { s.Env = map[string]string{"A=B": "x"} },
		"signal":       func(s *Spec) { s.StopSignal = "SIGNOPE" },
		"ulimit name":  func(s *Spec) { s.Rlimits = []Rlimit{{Name: "files", Soft: 1, Hard: 1}} },
		"ulimit order": func(s *Spec) { s.Rlimits = []Rlimit{{Name: "nofile", Soft: 10, Hard: 5}} },
		"ulimit inf":   func(s *Spec) { s.Rlimits = []Rlimit{{Name: "nofile", Soft: -1, Hard: 5}} },
		"hostname":     func(s *Spec) { s.Hostname = "a b" },
		"mtu":          func(s *Spec) { s.DockerMTU = &DockerMTU{MTU: 100} },
		"daemon json":  func(s *Spec) { s.DockerMTU = &DockerMTU{DaemonJSON: "daemon.json"} },
		"sha":          func(s *Spec) { s.BinarySHA256 = "abc" },
		"fallback sha": func(s *Spec) { s.FallbackSHA256 = strings.Repeat("G", 64) },
		"remote user":  func(s *Spec) { s.RemoteUser = "a:b" },
		"cgroups":      func(s *Spec) { s.Cgroups = "maybe" },
		"margin":       func(s *Spec) { s.MemoryMargin = -1 },
		"log dir":      func(s *Spec) { s.LogDir = "log" },
	} {
		s := validSpec()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestLoadSpec(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pid1.json")
	want := validSpec()
	data, _ := json.Marshal(want)
	data = bytes.Replace(data, []byte(`{"v":1`), []byte(`{"v":1,"newer_field":[1,2]`), 1)
	if err := os.WriteFile(file, data, 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSpec(file)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, want)
	}
	if DefaultSpecPath != "/opt/fleet/spec/pid1.json" {
		t.Fatal(DefaultSpecPath)
	}
	if _, err := LoadSpec(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("a missing spec loaded")
	}
}

func TestSignalNumber(t *testing.T) {
	for in, want := range map[string]int{
		"SIGTERM": 15, "term": 15, "15": 15, "SIGINT": 2, "SIGUSR1": 10, "sigwinch": 28, "SIGPWR": 30,
		"SIGRTMIN": 34, "SIGRTMIN+3": 37, "RTMAX": 64, "SIGRTMAX-2": 62, "64": 64,
	} {
		got, err := SignalNumber(in)
		if err != nil || got != want {
			t.Errorf("SignalNumber(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "65", "SIGNOPE", "SIGRTMIN+40", "RTMAX-40", "-1"} {
		if n, err := SignalNumber(bad); err == nil {
			t.Errorf("SignalNumber(%q) = %d, want an error", bad, n)
		}
	}
}

func TestRollingLogRotatesAndKeepsOffsets(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRolling(dir, 100, RunMark{ID: "one", PodUID: "uid-1"})
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	for i := range 20 {
		line := fmt.Sprintf("line %02d\n", i) // 8 bytes
		all.WriteString(line)
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	total := int64(all.Len())
	// 160 bytes in 50-byte segments: the oldest rotated away.
	fi, _ := os.Stat(filepath.Join(dir, RollingName))
	if fi.Size() > 50 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("current segment is %d bytes (over half the cap?), mode %v", fi.Size(), fi.Mode())
	}
	data, next, lost, err := ReadRollingSince(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != total || lost == 0 || !strings.HasSuffix(all.String(), string(data)) || int64(len(data))+lost != total || len(data) > 100 {
		t.Fatalf("since 0: %d bytes, next %d, lost %d", len(data), next, lost)
	}
	data, next, lost, _ = ReadRollingSince(dir, total-8)
	if string(data) != "line 19\n" || next != total || lost != 0 {
		t.Fatalf("since the last line: %q next %d lost %d", data, next, lost)
	}
	if data, next, _, _ := ReadRollingSince(dir, total); data != nil || next != total {
		t.Fatalf("since the end: %q %d", data, next)
	}

	// A second run starts where the first ended.
	r, err = OpenRolling(dir, 100, RunMark{ID: "two"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.Write([]byte("second run\n"))
	_ = r.Close()
	runs, err := ReadRolling(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "one" || runs[0].PodUID != "uid-1" || !runs[0].Truncated || runs[1].ID != "two" {
		t.Fatalf("runs %+v", runs)
	}
	if string(runs[1].Data) != "second run\n" || runs[1].Offset != total || runs[1].End != total+11 || runs[0].End != total {
		t.Fatalf("second run %+v / first ends %d", runs[1], runs[0].End)
	}
	if !strings.HasSuffix(all.String(), string(runs[0].Data)) {
		t.Fatalf("first run's data %q", runs[0].Data)
	}

	// fleetd parses the parts it fetched from the pod the same way.
	var parts RollingFiles
	for i, p := range []*[]byte{&parts.Index, &parts.Prev, &parts.Cur} {
		*p, _ = os.ReadFile(filepath.Join(dir, RollingDirFiles[i]))
	}
	remote := ParseRolling(parts)
	if got := remote.Runs(); len(got) != 2 || string(got[1].Data) != "second run\n" || remote.End != total+11 {
		t.Fatalf("parsed from fetched parts: %+v end %d", got, remote.End)
	}
	if data, lost := remote.Since(total); string(data) != "second run\n" || lost != 0 {
		t.Fatalf("since %d: %q %d", total, data, lost)
	}
	if empty := ParseRolling(RollingFiles{}); empty.Runs() != nil || empty.Start != 0 || empty.End != 0 {
		t.Fatalf("no parts: %+v", empty)
	}
}

func TestRollingLogNeverFailsItsWriter(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRolling(dir, 0, RunMark{ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.f.Close() // the disk went away under it
	if n, err := r.Write([]byte("still fine")); err != nil || n != 10 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if runs, err := ReadRolling(t.TempDir()); err != nil || runs != nil {
		t.Fatalf("an empty dir: %v %v", runs, err)
	}
}

func TestChooseBinary(t *testing.T) {
	bin := t.TempDir()
	write := func(content string) string {
		sha := sha256Hex(content)
		if err := os.WriteFile(filepath.Join(bin, podseed.BinaryName(sha)), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	cur, prev := write("current fleet"), write("previous fleet")
	other := sha256Hex("some other build")
	t.Setenv(reexecEnv, "")

	if c := chooseBinary(Spec{BinarySHA256: cur}, cur, bin); c.exec != "" || c.warning != "" {
		t.Fatalf("the right binary: %+v", c)
	}
	if c := chooseBinary(Spec{}, other, bin); c.exec != "" {
		t.Fatalf("no sha in the spec: %+v", c)
	}
	if c := chooseBinary(Spec{BinarySHA256: cur, FallbackSHA256: prev}, other, bin); c.exec != filepath.Join(bin, podseed.BinaryName(cur)) {
		t.Fatalf("a restage left the link elsewhere: %+v", c)
	}
	if err := os.Remove(filepath.Join(bin, podseed.BinaryName(cur))); err != nil {
		t.Fatal(err)
	}
	if c := chooseBinary(Spec{BinarySHA256: cur, FallbackSHA256: prev}, other, bin); c.exec != filepath.Join(bin, podseed.BinaryName(prev)) {
		t.Fatalf("the spec's binary is gone: %+v, want the fallback", c)
	}
	if c := chooseBinary(Spec{BinarySHA256: cur, FallbackSHA256: prev}, prev, bin); c.exec != "" || !strings.Contains(c.warning, "fallback") {
		t.Fatalf("already the fallback: %+v", c)
	}
	// A corrupt fleet-<sha> is not run.
	if err := os.WriteFile(filepath.Join(bin, podseed.BinaryName(cur)), []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := chooseBinary(Spec{BinarySHA256: cur}, other, bin); c.exec != "" || c.warning == "" {
		t.Fatalf("a corrupt candidate: %+v", c)
	}
	t.Setenv(reexecEnv, "1")
	if c := chooseBinary(Spec{BinarySHA256: cur, FallbackSHA256: prev}, other, bin); c.exec != "" {
		t.Fatalf("a re-exec'd pod-init chose again: %+v", c)
	}
}

func TestPasswdAppend(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nvscode:x:1000:1000::/home/vscode:/bin/zsh"
	line, ok := passwdEntry([]byte(passwd), 1000650000, 0, "vscode")
	if !ok || line != "fleet:x:1000650000:0:fleet pod user:/home/vscode:/bin/zsh" {
		t.Fatalf("%q %v", line, ok)
	}
	if _, ok := passwdEntry([]byte(passwd), 1000, 1000, "vscode"); ok {
		t.Fatal("a known uid got an entry")
	}
	line, _ = passwdEntry([]byte(passwd+"\nfleet:x:5:5::/:/bin/sh\n"), 77, 0, "nobody")
	if line != "fleet-77:x:77:0:fleet pod user:/:/bin/sh" {
		t.Fatalf("%q", line)
	}

	dir := t.TempDir()
	pf, gf := filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	_ = os.WriteFile(pf, []byte(passwd), 0o644) // no trailing newline
	_ = os.WriteFile(gf, []byte("root:x:0:\n"), 0o644)
	added, err := ensurePodUser(pf, gf, 1000650000, 1000650000, "vscode")
	if err != nil || len(added) != 2 {
		t.Fatalf("%v %v", added, err)
	}
	data, _ := os.ReadFile(pf)
	if !strings.HasSuffix(string(data), "/bin/zsh\nfleet:x:1000650000:1000650000:fleet pod user:/home/vscode:/bin/zsh\n") {
		t.Fatalf("passwd:\n%s", data)
	}
	g, _ := os.ReadFile(gf)
	if string(g) != "root:x:0:\nfleet:x:1000650000:\n" {
		t.Fatalf("group:\n%s", g)
	}
	if again, err := ensurePodUser(pf, gf, 1000650000, 1000650000, "vscode"); err != nil || len(again) != 0 {
		t.Fatalf("second run appended %v %v", again, err)
	}
}

func TestDockerMTU(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth1\t0000F40A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"net0\t00000000\t0100F40A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	if got := defaultRouteIface([]byte(table)); got != "net0" {
		t.Fatalf("default route iface %q", got)
	}
	if got := defaultRouteIface([]byte("Iface\tDestination\n")); got != "" {
		t.Fatalf("no default route: %q", got)
	}
	dir := t.TempDir()
	route := filepath.Join(dir, "route")
	_ = os.WriteFile(route, []byte(table), 0o644)
	mustMkdirAll(t, filepath.Join(dir, "net", "net0"))
	_ = os.WriteFile(filepath.Join(dir, "net", "net0", "mtu"), []byte("1450\n"), 0o644)
	oldRoute, oldNet := procNetRoute, sysClassNet
	procNetRoute, sysClassNet = route, filepath.Join(dir, "net")
	defer func() { procNetRoute, sysClassNet = oldRoute, oldNet }()
	if mtu, err := podMTU(); err != nil || mtu != 1450 {
		t.Fatalf("pod mtu %d %v", mtu, err)
	}

	cfg := filepath.Join(dir, "etc", "docker", "daemon.json")
	if changed, err := mergeDockerMTU(cfg, 1450); err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	if changed, err := mergeDockerMTU(cfg, 1450); err != nil || changed {
		t.Fatalf("no change: %v %v", changed, err)
	}
	_ = os.WriteFile(cfg, []byte(`{"registry-mirrors":["http://mirror"],"mtu":1500}`), 0o600)
	_ = os.Chmod(cfg, 0o600)
	if changed, err := mergeDockerMTU(cfg, 1450); err != nil || !changed {
		t.Fatalf("merge: %v %v", changed, err)
	}
	var got map[string]any
	data, _ := os.ReadFile(cfg)
	_ = json.Unmarshal(data, &got)
	if got["mtu"] != float64(1450) || got["registry-mirrors"] == nil {
		t.Fatalf("merged config %s", data)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	_ = os.WriteFile(cfg, []byte("null\n"), 0o644)
	if changed, err := mergeDockerMTU(cfg, 1450); err != nil || !changed {
		t.Fatalf("a null config: %v %v", changed, err)
	}
	_ = os.WriteFile(cfg, []byte("not json"), 0o644)
	if _, err := mergeDockerMTU(cfg, 1450); err == nil {
		t.Fatal("a broken config was overwritten")
	}
	if data, _ := os.ReadFile(cfg); string(data) != "not json" {
		t.Fatal("a broken config changed")
	}
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeCgroupFS is a cgroupfs in memory: files, dirs, and hooks for the
// kernel's refusals.
type fakeCgroupFS struct {
	files    map[string]string
	dirs     map[string]bool
	readOnly bool
	// busy makes this many subtree_control writes fail with EBUSY (a
	// process forked into the container cgroup meanwhile).
	busy   int
	writes []string
}

func newFakeCgroupFS(dir string, controllers, memMax string) *fakeCgroupFS {
	return &fakeCgroupFS{
		files: map[string]string{
			dir + "/cgroup.controllers": controllers,
			dir + "/cgroup.procs":       "1\n27\n",
			dir + "/memory.max":         memMax,
			dir + "/memory.oom.group":   "1",
		},
		dirs: map[string]bool{dir: true},
	}
}

func (f *fakeCgroupFS) ReadFile(name string) ([]byte, error) {
	v, ok := f.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(v), nil
}

func (f *fakeCgroupFS) WriteFile(name string, data []byte) error {
	if f.readOnly {
		return syscall.EROFS
	}
	f.writes = append(f.writes, path.Base(path.Dir(name))+"/"+path.Base(name)+"="+string(data))
	switch path.Base(name) {
	case "cgroup.subtree_control":
		if f.busy > 0 {
			f.busy--
			// The newcomer: a pid in the container cgroup again.
			f.files[path.Dir(name)+"/cgroup.procs"] = "99\n"
			return syscall.EBUSY
		}
	case "cgroup.procs":
		// Moving a process empties it from the container cgroup.
		for p, v := range f.files {
			if strings.HasSuffix(p, "/cgroup.procs") && !strings.Contains(p, "fleet-") {
				f.files[p] = strings.Replace(v, string(data)+"\n", "", 1)
			}
		}
	}
	if _, ok := f.files[name]; !ok && !f.dirs[path.Dir(name)] {
		return fs.ErrNotExist
	}
	f.files[name] = string(data)
	return nil
}

func (f *fakeCgroupFS) Mkdir(name string) error {
	if f.readOnly {
		return syscall.EROFS
	}
	if f.dirs[name] {
		return fs.ErrExist
	}
	f.dirs[name] = true
	return nil
}

func TestContainerCgroup(t *testing.T) {
	for in, want := range map[string]string{
		"0::/kubepods.slice/kubepods-burstable.slice/cri-containerd-abc.scope\n": "/kubepods.slice/kubepods-burstable.slice/cri-containerd-abc.scope",
		"0::/\n":                     "/",
		"12:memory:/x\n0::/y/../z\n": "/z",
	} {
		got, ok := containerCgroup([]byte(in))
		if !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := containerCgroup([]byte("12:memory:/docker/abc\n")); ok {
		t.Error("cgroup v1 read as v2")
	}
}

func TestSetupCgroups(t *testing.T) {
	const dir = "/sys/fs/cgroup/pod/ctr"
	f := newFakeCgroupFS(dir, "cpuset cpu io memory pids", "536870912\n")
	f.busy = 2
	l, err := setupCgroups(f, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !f.dirs[dir+"/fleet-agent"] || !f.dirs[dir+"/fleet-user"] {
		t.Fatalf("children not created: %v", f.dirs)
	}
	if !reflect.DeepEqual(l.Controllers, []string{"cpu", "memory", "pids"}) || f.files[dir+"/cgroup.subtree_control"] != "+cpu +memory +pids" {
		t.Fatalf("controllers %v, subtree_control %q", l.Controllers, f.files[dir+"/cgroup.subtree_control"])
	}
	if f.files[dir+"/cgroup.procs"] != "" {
		t.Fatalf("processes left in the container cgroup: %q (writes %v)", f.files[dir+"/cgroup.procs"], f.writes)
	}
	if !l.OOMGroup || f.files[dir+"/memory.oom.group"] != "0" {
		t.Fatal("memory.oom.group not set to 0")
	}
	want := int64(512<<20 - 64<<20)
	if l.UserMemoryMax != want || f.files[dir+"/fleet-user/memory.max"] != strconv.FormatInt(want, 10) {
		t.Fatalf("user memory.max %d (%q), want %d", l.UserMemoryMax, f.files[dir+"/fleet-user/memory.max"], want)
	}

	// The kubelet resizes the container and resets oom.group.
	f.files[dir+"/memory.oom.group"] = "1"
	f.files[dir+"/memory.max"] = "1073741824"
	notes := l.poll()
	if len(notes) != 2 || f.files[dir+"/memory.oom.group"] != "0" || l.UserMemoryMax != 1<<30-64<<20 {
		t.Fatalf("poll: %v, oom.group %q, user max %d", notes, f.files[dir+"/memory.oom.group"], l.UserMemoryMax)
	}
	if notes := l.poll(); len(notes) != 0 {
		t.Fatalf("a quiet poll: %v", notes)
	}
	f.files[dir+"/memory.max"] = "max\n"
	if l.poll(); l.UserMemoryMax != -1 || f.files[dir+"/fleet-user/memory.max"] != "max" {
		t.Fatalf("limit removed: %d %q", l.UserMemoryMax, f.files[dir+"/fleet-user/memory.max"])
	}

	file := filepath.Join(t.TempDir(), "run", "cgroup.json")
	if err := l.write(file); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLayout(file)
	if err != nil || got.User != dir+"/fleet-user" || got.Agent != dir+"/fleet-agent" || !got.OOMGroup {
		t.Fatalf("read back %+v %v", got, err)
	}
	if _, err := ReadLayout(filepath.Join(t.TempDir(), "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no layout: %v", err)
	}
}

func TestSetupCgroupsUnprivilegedAndStubborn(t *testing.T) {
	const dir = "/sys/fs/cgroup"
	ro := newFakeCgroupFS(dir, "cpu memory pids", "max")
	ro.readOnly = true
	if _, err := setupCgroups(ro, dir, 0); !errors.Is(err, errNoLayout) {
		t.Fatalf("a read-only cgroupfs: %v", err)
	}
	if _, err := setupCgroups(newFakeCgroupFS(dir, "", "max"), "/elsewhere", 0); !errors.Is(err, errNoLayout) {
		t.Fatalf("no cgroup v2 dir: %v", err)
	}
	busy := newFakeCgroupFS(dir, "memory", "max")
	busy.busy = moveRetries + 1
	if _, err := setupCgroups(busy, dir, 0); err == nil || errors.Is(err, errNoLayout) {
		t.Fatalf("processes that never stop arriving: %v", err)
	}
	// No limit: the user cgroup stays unlimited; no memory controller: none
	// enabled.
	none := newFakeCgroupFS(dir, "cpu pids", "max")
	l, err := setupCgroups(none, dir, 0)
	if err != nil || l.UserMemoryMax != -1 || !reflect.DeepEqual(l.Controllers, []string{"cpu", "pids"}) {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestMemoryMargin(t *testing.T) {
	for _, tc := range []struct{ limit, margin, want int64 }{
		{512 << 20, 0, 64 << 20},
		{2 << 30, 0, 64 << 20},
		{8 << 30, 0, 256 << 20},
		{64 << 30, 0, 256 << 20},
		{128 << 20, 0, 32 << 20},
		{1 << 30, 100 << 20, 100 << 20},
		{100 << 20, 90 << 20, 25 << 20},
	} {
		if got := memoryMargin(tc.limit, tc.margin); got != tc.want {
			t.Errorf("memoryMargin(%d, %d) = %d, want %d", tc.limit, tc.margin, got, tc.want)
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
