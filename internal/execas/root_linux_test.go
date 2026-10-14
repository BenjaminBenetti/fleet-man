//go:build linux

package execas

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The TestAsRoot tests switch users for real, so they need root: run the test
// binary as root (CI's unit job does, with FLEET_EXECAS_REQUIRE_ROOT=1, which
// turns their skip into a failure so they cannot silently stop running).
//
//	go test -c -o /tmp/execas.test ./internal/execas
//	sudo FLEET_EXECAS_REQUIRE_ROOT=1 /tmp/execas.test -test.run AsRoot -test.v
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 && CanSwitchUser() {
		return
	}
	if os.Getenv("FLEET_EXECAS_REQUIRE_ROOT") == "1" {
		t.Fatal("FLEET_EXECAS_REQUIRE_ROOT is set but this is not root with CAP_SETUID and CAP_SETGID")
	}
	t.Skip("needs root; see requireRoot")
}

// rootPod is a root pod whose image has a remoteUser, a containerUser and a
// tty group, each user with a real home it owns, and whose runtime added an
// fsGroup (7777) to the container.
type rootPod struct {
	host      *Host
	bin       string
	workspace string
	homes     map[string]string
}

func newRootPod(t *testing.T) *rootPod {
	t.Helper()
	bin := publicCopy(t)
	pub := func() string {
		d := t.TempDir()
		_ = os.Chmod(d, 0o755)
		return d
	}
	workspace, workdir := pub(), pub()
	homes := map[string]string{"dev": pub(), "app": pub()}
	for name, uid := range map[string]int{"dev": 4242, "app": 4343} {
		if err := os.Chown(homes[name], uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(homes["dev"], ".profile")
	if err := os.WriteFile(profile, []byte("export PROBED_AS=$(id -u):$(id -g)\nexport PATH=\"$HOME/bin:$PATH\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(profile, 4242, 4242); err != nil {
		t.Fatal(err)
	}
	passwd := fmt.Sprintf(`root:x:0:0:root:/root:/bin/sh
dev:x:4242:4242::%s:/bin/sh
app:x:4343:4343::%s:/bin/sh
`, homes["dev"], homes["app"])
	group := `root:x:0:
tty:x:5:
dev:x:4242:
app:x:4343:
docker:x:998:dev,app
sudo:x:27:dev
`
	return &rootPod{
		bin:       bin,
		workspace: workspace,
		homes:     homes,
		host: &Host{
			Root: writeRoot(t, passwd, group),
			Spec: Spec{
				RemoteUser:      "dev",
				ContainerUser:   "app",
				WorkspaceFolder: workspace,
				WorkingDir:      workdir,
				UserEnvProbe:    ProbeLoginShell,
				RemoteEnv:       map[string]string{"FROM_REMOTE_ENV": "1"},
			},
			ProcessEnv: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"},
			Self:       Creds{UID: 0, GID: 0, Groups: []uint32{0, 7777}},
			Prober:     NewProber(""),
		},
	}
}

// run starts req's report helper and returns what it saw.
func (p *rootPod) run(t *testing.T, req Request) (helperReport, *Plan) {
	t.Helper()
	req.Argv = []string{p.bin}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	req.Env[helperEnv] = "report"
	plan := mustPlan(t, p.host, req)
	var buf bytes.Buffer
	proc, err := Start(plan, Stdio{Stdout: &buf})
	if err != nil {
		t.Fatal(err)
	}
	out := []byte(nil)
	if plan.TTY {
		out, _ = io.ReadAll(proc.PTY())
		defer proc.PTY().Close()
	}
	st, err := proc.Wait()
	if err != nil || st.Code != 0 {
		t.Fatalf("Wait = %+v, %v", st, err)
	}
	if !plan.TTY {
		out = buf.Bytes()
	}
	return parseReport(t, out), plan
}

func TestAsRootIdentityMatrix(t *testing.T) {
	requireRoot(t)
	pod := newRootPod(t)
	for _, tc := range []struct {
		name     string
		req      Request
		uid, gid int
		groups   []int
		home     string
		cwd      string
		probed   bool
	}{
		{"exec is the remoteUser", Request{Class: ClassExec}, 4242, 4242, []int{4242, 998, 27, 7777}, pod.homes["dev"], pod.workspace, true},
		{"script is the same user", Request{Class: ClassScript}, 4242, 4242, []int{4242, 998, 27, 7777}, pod.homes["dev"], pod.host.Spec.WorkingDir, false},
		{"explicit defaults to containerUser", Request{Class: ClassExplicit}, 4343, 4343, []int{4343, 998, 7777}, pod.homes["app"], pod.host.Spec.WorkingDir, false},
		{"explicit with user:group", Request{Class: ClassExplicit, User: "dev:998", Cwd: "/"}, 4242, 998, []int{998, 7777}, pod.homes["dev"], "/", false},
		{"explicit with a bare uid", Request{Class: ClassExplicit, User: "5151"}, 5151, 0, []int{0, 7777}, "/", pod.host.Spec.WorkingDir, false},
		{"root", Request{Class: ClassRoot}, 0, 0, []int{0, 7777}, "/root", pod.host.Spec.WorkingDir, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := pod.run(t, tc.req)
			if r.UID != tc.uid || r.EUID != tc.uid || r.GID != tc.gid {
				t.Errorf("ran as uid=%d euid=%d gid=%d, want %d:%d", r.UID, r.EUID, r.GID, tc.uid, tc.gid)
			}
			got := slices.Clone(r.Groups)
			want := slices.Clone(tc.groups)
			slices.Sort(got)
			slices.Sort(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("groups = %v, want %v (initgroups plus the runtime's 7777, none of root's)", r.Groups, tc.groups)
			}
			if !hasEnv(r.Env, "HOME="+tc.home) {
				t.Errorf("HOME is not %s: %v", tc.home, r.Env)
			}
			if r.Cwd != tc.cwd {
				t.Errorf("cwd = %q, want %q", r.Cwd, tc.cwd)
			}
			probed := hasEnv(r.Env, "PROBED_AS=4242:4242")
			if probed != tc.probed {
				t.Errorf("probed env present = %v, want %v: %v", probed, tc.probed, r.Env)
			}
			if tc.probed && !hasEnv(r.Env, "FROM_REMOTE_ENV=1") {
				t.Errorf("remoteEnv missing: %v", r.Env)
			}
		})
	}
}

func TestAsRootPTYBelongsToTheUser(t *testing.T) {
	requireRoot(t)
	pod := newRootPod(t)
	r, plan := pod.run(t, Request{Class: ClassExec, TTY: true, Rows: 30, Cols: 90})
	if r.TTY == nil {
		t.Fatal("stdin is not a terminal")
	}
	if r.TTY.UID != 4242 || r.TTY.GID != 5 || r.TTY.Mode != 0o620 {
		t.Errorf("pty %d:%d %o, want 4242:5 (tty) 0620", r.TTY.UID, r.TTY.GID, r.TTY.Mode)
	}
	if !r.TTY.Ctty || r.TTY.Rows != 30 || r.TTY.Cols != 90 || r.UID != 4242 {
		t.Errorf("tty %+v uid %d", r.TTY, r.UID)
	}
	if plan.TTYGroup != 5 {
		t.Errorf("TTYGroup = %d", plan.TTYGroup)
	}
}

func TestAsRootLookPathAsTheUser(t *testing.T) {
	requireRoot(t)
	pod := newRootPod(t)
	// root can run a 0700 root file; the remoteUser cannot, so the lookup skips
	// it for a later PATH entry, as a shell running as that user would.
	rootOnly, public := t.TempDir(), t.TempDir()
	_ = os.Chmod(rootOnly, 0o755)
	_ = os.Chmod(public, 0o755)
	writeExecutable(t, filepath.Join(rootOnly, "tool"), 0o700)
	writeExecutable(t, filepath.Join(public, "tool"), 0o755)
	pod.host.ProcessEnv = []string{"PATH=" + rootOnly + ":" + public}
	pod.host.Spec.UserEnvProbe = ProbeNone
	plan := mustPlan(t, pod.host, Request{Class: ClassScript, Argv: []string{"tool"}})
	if plan.Path != filepath.Join(public, "tool") {
		t.Errorf("as dev: Path = %q, want the one dev may run", plan.Path)
	}
	plan = mustPlan(t, pod.host, Request{Class: ClassRoot, Argv: []string{"tool"}})
	if plan.Path != filepath.Join(rootOnly, "tool") {
		t.Errorf("as root: Path = %q, want the first", plan.Path)
	}
}

// TestAsRootExecAs runs `fleet exec-as` as root against the real /etc: it
// takes the user in-process and execs the command in its place.
func TestAsRootExecAs(t *testing.T) {
	requireRoot(t)
	bin := publicCopy(t)
	work := t.TempDir()
	_ = os.Chmod(work, 0o755)
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil { // t.TempDir's own subdirs are 0755
		t.Fatal(err)
	}
	spec := writeSpecFile(t, `{"working_dir":"`+work+`","user_env_probe":"none","container_user":"6161:6262"}`)

	stdin := append(frame(t, Request{Class: ClassExplicit, Argv: []string{bin},
		Env: map[string]string{helperEnv: "report", "EXECAS_TEST_STDIN": "1"}}), []byte("payload")...)
	stdout, stderr, state := runExecAs(t, spec, bytes.NewReader(stdin))
	if state.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", state.ExitCode(), stderr)
	}
	r := parseReport(t, []byte(stdout))
	if r.UID != 6161 || r.EUID != 6161 || r.GID != 6262 || r.Groups[0] != 6262 {
		t.Errorf("ran as uid=%d euid=%d gid=%d groups=%v, want 6161:6262", r.UID, r.EUID, r.GID, r.Groups)
	}
	if r.Pid != state.Pid() || r.Stdin != "payload" || r.Cwd != work || !hasEnv(r.Env, "HOME=/") {
		t.Errorf("pid %d (exec-as %d) stdin %q cwd %q env %v", r.Pid, state.Pid(), r.Stdin, r.Cwd, r.Env)
	}

	// The working directory is entered as the user, so one only root may enter
	// is refused, as docker refuses it.
	_, stderr, state = runExecAs(t, spec, bytes.NewReader(frame(t, Request{Class: ClassExplicit, Argv: []string{"/bin/true"}, Cwd: private})))
	if state.ExitCode() != ExitCannotRun || !strings.Contains(stderr, "chdir "+private+": permission denied") {
		t.Errorf("private cwd: exit %d stderr %q", state.ExitCode(), stderr)
	}

	// The root class stays root.
	stdout, stderr, state = runExecAs(t, spec, bytes.NewReader(frame(t, Request{Class: ClassRoot, Argv: []string{"/usr/bin/id", "-u"}})))
	if state.ExitCode() != 0 || strings.TrimSpace(stdout) != "0" {
		t.Errorf("root class: exit %d out %q err %q", state.ExitCode(), stdout, stderr)
	}
}
