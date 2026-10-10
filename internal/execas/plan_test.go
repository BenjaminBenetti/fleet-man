package execas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubProber answers a probe the way a login shell would: it keeps the
// environment it started with, prepends a directory of its own to PATH and
// says whom it ran as.
type stubProber struct {
	mu      sync.Mutex
	bin     string
	targets []ProbeTarget
}

func (s *stubProber) ProbeEnv(_ context.Context, t ProbeTarget) map[string]string {
	s.mu.Lock()
	s.targets = append(s.targets, t)
	s.mu.Unlock()
	start := envMap(t.Env)
	return map[string]string{
		"PATH":       s.bin + ":" + start["PATH"],
		"HOME":       start["HOME"],
		"PROBED_UID": strconv.Itoa(int(t.Identity.UID)),
		"FROM_RC":    "rc",
	}
}

func envMap(entries []string) map[string]string {
	m := make(map[string]string)
	for _, e := range entries {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// matrixHost is a pod as the identity matrix sees it: the matrix /etc, a
// devcontainer spec, and a bin dir holding "tool" on the container PATH and
// "probed-tool" only on the PATH the user's startup files set.
type matrixHost struct {
	host   *Host
	prober *stubProber
	bin    string
}

func newMatrixHost(t *testing.T) *matrixHost {
	t.Helper()
	bin := t.TempDir()
	probed := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "tool"), 0o755)
	writeExecutable(t, filepath.Join(probed, "probed-tool"), 0o755)
	prober := &stubProber{bin: probed}
	return &matrixHost{
		bin:    bin,
		prober: prober,
		host: &Host{
			Root: writeRoot(t, matrixPasswd, matrixGroup),
			Spec: Spec{
				RemoteUser:      "vscode",
				ContainerUser:   "node",
				WorkspaceFolder: "/workspaces/proj",
				WorkingDir:      "/srv",
				RemoteEnv: map[string]string{
					"EDITOR":   "vim",
					"CPATH":    "${containerEnv:PATH}:/remote",
					"FALLBACK": "${containerEnv:UNSET:dflt}",
					"LOCAL":    "${localEnv:USER}",
					"FROM_RC":  "remote wins over the probe",
				},
				SSHAuthSock: "/fleet-mounts/control/ssh-agent.sock",
			},
			// What the agent's kube exec got: the container env plus the HOME
			// runc set for root, the exec user.
			ProcessEnv: []string{"PATH=" + bin + ":/usr/bin:/bin", "HOSTNAME=fi-x-0", "LANG=C.UTF-8", "HOME=/root"},
			// Root, with an fsGroup the runtime added.
			Self:   Creds{UID: 0, GID: 0, Groups: []uint32{0, 4000}},
			Prober: prober,
		},
	}
}

func writeExecutable(t *testing.T, file string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
}

type planWant struct {
	uid, gid uint32
	groups   []uint32
	name     string
	home     string
	dir      string
	switched bool
	// env holds variables the plan must set to exactly these values; unset
	// names variables it must not set.
	env   map[string]string
	unset []string
	// probedAs is the uid the probe ran as; -1 means no probe ran.
	probedAs int
}

func checkPlan(t *testing.T, m *matrixHost, plan *Plan, want planWant) {
	t.Helper()
	id := plan.Identity
	if id.UID != want.uid || id.GID != want.gid || !reflect.DeepEqual(id.Groups, want.groups) {
		t.Errorf("identity uid=%d gid=%d groups=%v, want uid=%d gid=%d groups=%v",
			id.UID, id.GID, id.Groups, want.uid, want.gid, want.groups)
	}
	if id.Name != want.name || id.Home != want.home {
		t.Errorf("identity name=%q home=%q, want name=%q home=%q", id.Name, id.Home, want.name, want.home)
	}
	if plan.Dir != want.dir {
		t.Errorf("dir = %q, want %q", plan.Dir, want.dir)
	}
	if plan.Switch != want.switched {
		t.Errorf("switch = %v, want %v", plan.Switch, want.switched)
	}
	env := envMap(plan.Env)
	if len(env) != len(plan.Env) {
		t.Errorf("duplicate variables in %v", plan.Env)
	}
	for k, v := range want.env {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("env %s = %q (set %v), want %q", k, got, ok, v)
		}
	}
	for _, k := range want.unset {
		if v, ok := env[k]; ok {
			t.Errorf("env %s = %q, want it unset", k, v)
		}
	}
	switch {
	case want.probedAs < 0 && len(m.prober.targets) != 0:
		t.Errorf("probed %d times, want no probe", len(m.prober.targets))
	case want.probedAs >= 0 && len(m.prober.targets) != 1:
		t.Errorf("probed %d times, want once", len(m.prober.targets))
	case want.probedAs >= 0 && m.prober.targets[0].Identity.UID != uint32(want.probedAs):
		t.Errorf("probed as uid %d, want %d", m.prober.targets[0].Identity.UID, want.probedAs)
	}
}

// TestIdentityMatrix is every class against a root pod (switching users) and a
// tier C pod (single uid), with docker's answer for each as the expectation.
func TestIdentityMatrix(t *testing.T) {
	const podUID = 1000650000
	notExec := []string{"SSH_AUTH_SOCK", "EDITOR", "CPATH", "PROBED_UID", "FROM_RC", "TERM"}
	for _, tc := range []struct {
		name  string
		setup func(m *matrixHost)
		req   Request
		want  planWant
	}{
		{
			name: "exec runs as remoteUser with its probed env in the workspace",
			req:  Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/workspaces/proj", switched: true, probedAs: 1000,
				env: map[string]string{
					"HOME":          "/home/vscode",
					"PROBED_UID":    "1000",
					"SSH_AUTH_SOCK": "/fleet-mounts/control/ssh-agent.sock",
					"EDITOR":        "vim",
					"FALLBACK":      "dflt",
					"LOCAL":         "${localEnv:USER}",
					"FROM_RC":       "remote wins over the probe",
					"HOSTNAME":      "fi-x-0",
					"LANG":          "C.UTF-8",
				},
				unset: []string{"TERM", "PWD"},
			},
		},
		{
			name: "script runs as the same user with the bare container env",
			req:  Request{Class: ClassScript, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/srv", switched: true, probedAs: -1,
				env:   map[string]string{"HOME": "/home/vscode", "HOSTNAME": "fi-x-0"},
				unset: notExec,
			},
		},
		{
			name: "explicit defaults to the container's user and dir",
			req:  Request{Class: ClassExplicit, Argv: []string{"tool"}},
			want: planWant{
				uid: 1001, gid: 1001, groups: []uint32{1001, 998, 4000}, name: "node", home: "/home/node",
				dir: "/srv", switched: true, probedAs: -1,
				env: map[string]string{"HOME": "/home/node"}, unset: notExec,
			},
		},
		{
			name: "explicit takes the shim's -u user:group, -w and -e",
			req: Request{Class: ClassExplicit, Argv: []string{"tool"}, User: "svc:media", Cwd: "/tmp",
				Env: map[string]string{"HOOK": "postCreate", "HOME": "/override"}},
			want: planWant{
				uid: 2000, gid: 3000, groups: []uint32{3000, 4000}, name: "svc", home: "/srv/svc",
				dir: "/tmp", switched: true, probedAs: -1,
				env: map[string]string{"HOOK": "postCreate", "HOME": "/override"},
			},
		},
		{
			name: "root runs as uid 0 with root's home",
			req:  Request{Class: ClassRoot, Argv: []string{"tool"}},
			want: planWant{
				uid: 0, gid: 0, groups: []uint32{0, 4000}, name: "root", home: "/root",
				dir: "/srv", switched: true, probedAs: -1,
				env: map[string]string{"HOME": "/root"}, unset: notExec,
			},
		},
		{
			name: "exec with no remoteUser falls back to containerUser",
			setup: func(m *matrixHost) {
				m.host.Spec.RemoteUser = ""
			},
			req: Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: 1001, gid: 1001, groups: []uint32{1001, 998, 4000}, name: "node", home: "/home/node",
				dir: "/workspaces/proj", switched: true, probedAs: 1001,
				env: map[string]string{"HOME": "/home/node"},
			},
		},
		{
			name: "exec with no users at all runs as root",
			setup: func(m *matrixHost) {
				m.host.Spec.RemoteUser, m.host.Spec.ContainerUser = "", ""
			},
			req: Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: 0, gid: 0, groups: []uint32{0, 4000}, name: "root", home: "/root",
				dir: "/workspaces/proj", switched: true, probedAs: 0,
				env: map[string]string{"HOME": "/root"},
			},
		},
		{
			name: "a numeric remoteUser without an entry runs as that uid, gid 0, home /",
			setup: func(m *matrixHost) {
				m.host.Spec.RemoteUser = "4242"
			},
			req: Request{Class: ClassScript, Argv: []string{"tool"}},
			want: planWant{
				uid: 4242, gid: 0, groups: []uint32{0, 4000}, home: "/",
				dir: "/srv", switched: true, probedAs: -1,
				env: map[string]string{"HOME": "/"},
			},
		},
		{
			name: "a request user overrides the exec class's",
			req:  Request{Class: ClassExec, Argv: []string{"tool"}, User: "node"},
			want: planWant{
				uid: 1001, gid: 1001, groups: []uint32{1001, 998, 4000}, name: "node", home: "/home/node",
				dir: "/workspaces/proj", switched: true, probedAs: 1001,
				env: map[string]string{"HOME": "/home/node"},
			},
		},
		{
			name: "a TTY defaults TERM to xterm, as docker does",
			req:  Request{Class: ClassScript, Argv: []string{"tool"}, TTY: true, Rows: 24, Cols: 80},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/srv", switched: true, probedAs: -1,
				env: map[string]string{"TERM": "xterm"},
			},
		},
		{
			name: "the client's TERM wins over the default",
			req:  Request{Class: ClassExec, Argv: []string{"tool"}, TTY: true, Env: map[string]string{"TERM": "xterm-256color", "EDITOR": "nano"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/workspaces/proj", switched: true, probedAs: 1000,
				env: map[string]string{"TERM": "xterm-256color", "EDITOR": "nano"},
			},
		},
		{
			name: "an image HOME is kept for every user",
			setup: func(m *matrixHost) {
				m.host.ProcessEnv = append(m.host.ProcessEnv, "HOME=/opt/imagehome")
			},
			req: Request{Class: ClassScript, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/srv", switched: true, probedAs: -1,
				env: map[string]string{"HOME": "/opt/imagehome"},
			},
		},
		{
			name: "userEnvProbe none skips the probe but keeps remoteEnv",
			setup: func(m *matrixHost) {
				m.host.Spec.UserEnvProbe = ProbeNone
			},
			req: Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/workspaces/proj", switched: true, probedAs: -1,
				env:   map[string]string{"EDITOR": "vim", "HOME": "/home/vscode"},
				unset: []string{"PROBED_UID"},
			},
		},
		{
			name: "exec without a workspace folder starts in the user's home",
			setup: func(m *matrixHost) {
				m.host.Spec.WorkspaceFolder = ""
			},
			req: Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000, gid: 1000, groups: []uint32{1000, 27, 998, 4000}, name: "vscode", home: "/home/vscode",
				dir: "/home/vscode", switched: true, probedAs: 1000,
			},
		},
		{
			name: "no container working dir means /",
			setup: func(m *matrixHost) {
				m.host.Spec.WorkingDir = ""
			},
			req: Request{Class: ClassRoot, Argv: []string{"tool"}},
			want: planWant{
				uid: 0, gid: 0, groups: []uint32{0, 4000}, name: "root", home: "/root",
				dir: "/", switched: true, probedAs: -1,
			},
		},

		// Tier C: the pod runs as an arbitrary uid pod-init appended to
		// /etc/passwd, and every class runs as it, whoever the class names.
		{
			name:  "tier C exec runs as the pod uid, probe included",
			setup: tierC(podUID),
			req:   Request{Class: ClassExec, Argv: []string{"tool"}},
			want: planWant{
				uid: podUID, gid: 0, groups: []uint32{0, podUID}, name: "fleetpod", home: "/home/fleetpod",
				dir: "/workspaces/proj", switched: false, probedAs: podUID,
				env: map[string]string{"HOME": "/home/fleetpod", "SSH_AUTH_SOCK": "/fleet-mounts/control/ssh-agent.sock", "EDITOR": "vim"},
			},
		},
		{
			name:  "tier C script runs as the pod uid",
			setup: tierC(podUID),
			req:   Request{Class: ClassScript, Argv: []string{"tool"}},
			want: planWant{
				uid: podUID, gid: 0, groups: []uint32{0, podUID}, name: "fleetpod", home: "/home/fleetpod",
				dir: "/srv", switched: false, probedAs: -1, env: map[string]string{"HOME": "/home/fleetpod"}, unset: notExec,
			},
		},
		{
			name:  "tier C root runs as the pod uid",
			setup: tierC(podUID),
			req:   Request{Class: ClassRoot, Argv: []string{"tool"}},
			want: planWant{
				uid: podUID, gid: 0, groups: []uint32{0, podUID}, name: "fleetpod", home: "/home/fleetpod",
				dir: "/srv", switched: false, probedAs: -1,
			},
		},
		{
			name:  "tier C explicit ignores the shim's user",
			setup: tierC(podUID),
			req:   Request{Class: ClassExplicit, Argv: []string{"tool"}, User: "svc:media", Cwd: "/tmp"},
			want: planWant{
				uid: podUID, gid: 0, groups: []uint32{0, podUID}, name: "fleetpod", home: "/home/fleetpod",
				dir: "/tmp", switched: false, probedAs: -1,
			},
		},
		{
			name: "tier C with no passwd entry for the pod uid gets home /",
			setup: func(m *matrixHost) {
				tierC(1000660000)(m)
			},
			req: Request{Class: ClassScript, Argv: []string{"tool"}},
			want: planWant{
				uid: 1000660000, gid: 0, groups: []uint32{0, 1000660000}, home: "/",
				dir: "/srv", switched: false, probedAs: -1, env: map[string]string{"HOME": "/"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMatrixHost(t)
			if tc.setup != nil {
				tc.setup(m)
			}
			plan, err := m.host.Plan(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			checkPlan(t, m, plan, tc.want)
			if plan.Class != tc.req.Class || !reflect.DeepEqual(plan.Argv, tc.req.Argv) || plan.TTY != tc.req.TTY ||
				plan.Rows != tc.req.Rows || plan.Cols != tc.req.Cols {
				t.Errorf("plan does not carry the request: %+v", plan)
			}
		})
	}
}

// tierC makes the matrix pod a no-root one running as uid.
func tierC(uid uint32) func(m *matrixHost) {
	return func(m *matrixHost) {
		m.host.SingleUID = true
		m.host.Self = Creds{UID: uid, GID: 0, Groups: []uint32{0, uid}}
		m.host.ProcessEnv = slices.DeleteFunc(m.host.ProcessEnv, func(e string) bool { return strings.HasPrefix(e, "HOME=") })
		// The runtime's HOME for the pod uid: its passwd home, else /.
		users, _ := ParsePasswd(strings.NewReader(matrixPasswd))
		db := &DB{Users: users}
		m.host.ProcessEnv = append(m.host.ProcessEnv, "HOME="+db.identityOf(m.host.Self).Home)
	}
}

func TestPlanEnvironmentLayers(t *testing.T) {
	m := newMatrixHost(t)
	m.host.Spec.WorkingDir = "/no/such/workdir"
	plan, err := m.host.Plan(context.Background(), Request{Class: ClassExec, Argv: []string{"probed-tool"},
		Env: map[string]string{"EDITOR": "request wins"}})
	if err != nil {
		t.Fatal(err)
	}
	env := envMap(plan.Env)

	// The probe started from the container env, with the user's HOME and no
	// SSH_AUTH_SOCK or remoteEnv (the CLI's probe is a bare docker exec).
	start := envMap(m.prober.targets[0].Env)
	if start["HOME"] != "/home/vscode" || start["PATH"] != m.bin+":/usr/bin:/bin" {
		t.Errorf("probe start env = %v", start)
	}
	for _, k := range []string{"SSH_AUTH_SOCK", "EDITOR", "TERM"} {
		if _, ok := start[k]; ok {
			t.Errorf("probe start env has %s", k)
		}
	}
	if got := m.prober.targets[0]; got.Shell != "/bin/zsh" || got.Dir != "/" || got.Mode != ProbeLoginInteractiveShell || !got.Switch {
		t.Errorf("probe target = %+v, want vscode's zsh, login-interactive, in / (the working dir is missing)", got)
	}

	// remoteEnv's ${containerEnv:PATH} is the container's PATH, not the probed one.
	if want := m.bin + ":/usr/bin:/bin:/remote"; env["CPATH"] != want {
		t.Errorf("CPATH = %q, want %q", env["CPATH"], want)
	}
	// The executable is found on the probed PATH, as `devcontainer exec` finds it.
	if plan.Path != filepath.Join(m.prober.bin, "probed-tool") {
		t.Errorf("Path = %q, want the probed PATH's", plan.Path)
	}
	if env["EDITOR"] != "request wins" {
		t.Errorf("EDITOR = %q: the request's env is the last layer", env["EDITOR"])
	}
	if plan.TTYGroup != 5 {
		t.Errorf("TTYGroup = %d, want the tty group's 5", plan.TTYGroup)
	}
}

func TestPlanUsesTheContainerShellAndDefaultsPath(t *testing.T) {
	m := newMatrixHost(t)
	m.host.ProcessEnv = []string{"SHELL=/bin/fish", "HOME=/root"}
	m.host.Spec.WorkingDir = m.bin // exists, so the probe starts there
	plan, err := m.host.Plan(context.Background(), Request{Class: ClassExec, Argv: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	target := m.prober.targets[0]
	if target.Shell != "/bin/fish" || target.Dir != m.bin {
		t.Errorf("probe shell %q dir %q, want the container's SHELL and working dir", target.Shell, target.Dir)
	}
	if envMap(target.Env)["PATH"] != DefaultPath {
		t.Errorf("probe PATH = %q, want docker's default", envMap(target.Env)["PATH"])
	}
	if !strings.HasSuffix(envMap(plan.Env)["PATH"], DefaultPath) {
		t.Errorf("PATH = %q", envMap(plan.Env)["PATH"])
	}
}

func TestPlanWithoutATTYGroupChownsToTheUsersGroup(t *testing.T) {
	m := newMatrixHost(t)
	m.host.Root = writeRoot(t, matrixPasswd, strings.ReplaceAll(matrixGroup, "tty:x:5:\n", ""))
	plan, err := m.host.Plan(context.Background(), Request{Class: ClassScript, Argv: []string{"tool"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TTYGroup != 1000 {
		t.Fatalf("TTYGroup = %d, want vscode's 1000", plan.TTYGroup)
	}
}

func TestPlanFailuresCarryDockersExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *matrixHost)
		req   Request
		code  int
		want  string
	}{
		{"unknown remoteUser", func(m *matrixHost) { m.host.Spec.RemoteUser = "ghost" },
			Request{Class: ClassExec, Argv: []string{"tool"}}, ExitCannotRun, "unable to find user ghost"},
		{"unknown request user", nil,
			Request{Class: ClassExplicit, Argv: []string{"tool"}, User: "nobody2"}, ExitCannotRun, "unable to find user"},
		{"root class with a user", nil,
			Request{Class: ClassRoot, Argv: []string{"tool"}, User: "vscode"}, ExitFailed, "runs as root"},
		{"missing command", nil,
			Request{Class: ClassScript, Argv: []string{"no-such-tool"}}, ExitNotFound, "executable file not found"},
		{"missing path", nil,
			Request{Class: ClassScript, Argv: []string{"/no/such/tool"}}, ExitNotFound, "no such file"},
		{"bad remoteEnv reference", func(m *matrixHost) { m.host.Spec.RemoteEnv = map[string]string{"X": "${containerEnv}"} },
			Request{Class: ClassExec, Argv: []string{"tool"}}, ExitFailed, "remoteEnv X"},
		{"invalid spec", func(m *matrixHost) { m.host.Spec.UserEnvProbe = "bogus" },
			Request{Class: ClassScript, Argv: []string{"tool"}}, ExitFailed, "exec spec"},
		{"unreadable passwd", func(m *matrixHost) {
			m.host.Root = t.TempDir()
			_ = os.MkdirAll(filepath.Join(m.host.Root, "etc", "passwd"), 0o755)
		}, Request{Class: ClassScript, Argv: []string{"tool"}}, ExitFailed, "passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMatrixHost(t)
			if tc.setup != nil {
				tc.setup(m)
			}
			_, err := m.host.Plan(context.Background(), tc.req)
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != tc.code || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want exit %d containing %q", err, tc.code, tc.want)
			}
		})
	}
}

// ctxProber blocks until its caller's context ends, as a probe whose shell is
// still starting would.
type ctxProber struct{}

func (ctxProber) ProbeEnv(ctx context.Context, _ ProbeTarget) map[string]string {
	<-ctx.Done()
	return nil
}

func TestPlanCancelledDuringTheProbeFails(t *testing.T) {
	m := newMatrixHost(t)
	m.host.Prober = ctxProber{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	plan, err := m.host.Plan(ctx, Request{Class: ClassExec, Argv: []string{"tool"}})
	if plan != nil || ExitCode(err) != ExitFailed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plan %v, err %v: want no plan and exit %d for a request cancelled mid-probe", plan, err, ExitFailed)
	}
}

func TestPlanWithoutAProberRunsNoProbe(t *testing.T) {
	m := newMatrixHost(t)
	m.host.Prober = nil
	plan, err := m.host.Plan(context.Background(), Request{Class: ClassExec, Argv: []string{"tool"}})
	if err != nil {
		t.Fatal(err)
	}
	if env := envMap(plan.Env); env["EDITOR"] != "vim" || env["PROBED_UID"] != "" {
		t.Fatalf("env = %v", env)
	}
}
