package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

// runStep runs one step of the audio script (both directions on) in env as a
// session user whose home is home, with the staged fleet binary present.
func (env *micScriptEnv) runStep(t *testing.T, body, home string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(env.stubBin, "sh"), "-c", body)
	cmd.Env = []string{"PATH=" + env.stubBin, "FLEET_MIC_ROOT=" + env.root, "HOME=" + home}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stageFleet stages a fleet binary that records each call (mic/output ensure).
func (env *micScriptEnv) stageFleet(t *testing.T) {
	t.Helper()
	env.writeRoot(t, "usr/bin/fleet", "#!/bin/sh\necho \"fleet $*\" >> \""+env.log+"\"\nexit 0\n")
	if err := os.Chmod(filepath.Join(env.root, "usr/bin/fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The System step (run as root) installs and configures and stops there: the
// sound server is the session user's, so it starts nothing.
func TestAudioSystemStepInstallsAndStartsNothing(t *testing.T) {
	env := newMicScriptEnv(t)
	env.stageFleet(t)
	env.packageManager(t, "apt-get", env.audioInstallSnippet()+`; printf '#!/bin/sh\nexit 0\n' > "`+env.stubBin+`/parec"; chmod +x "`+env.stubBin+`/parec"`)
	out, err := env.runStep(t, AudioScript(true, true).Privileged, t.TempDir())
	if err != nil {
		t.Fatalf("system step: %v\n%s", err, out)
	}
	if !strings.Contains(out, "audio packages and configuration installed") {
		t.Fatalf("output: %s", out)
	}
	calls := env.calls(t)
	if !strings.Contains(calls, "apt-get") || strings.Contains(calls, "fleet ") {
		t.Fatalf("the system step should install, and run no fleet ensure:\n%s", calls)
	}
	if !strings.Contains(env.read(t, "etc/asound.conf"), "pcm.!default { type pulse }") || env.read(t, "etc/pulse/client.conf.d/00-fleet-mic.conf") == "" {
		t.Fatal("the system step did not write fleet's /etc configuration")
	}
}

// The User step after it finds the system-wide configuration in place: it
// installs nothing, writes nothing, and brings the session user's server up.
func TestAudioUserStepAfterTheSystemStepOnlyStartsTheServer(t *testing.T) {
	env := newMicScriptEnv(t)
	env.installAudio(t)
	env.stageFleet(t)
	env.packageManager(t, "apt-get", "exit 99")
	if out, err := env.runStep(t, AudioScript(true, true).Privileged, t.TempDir()); err != nil {
		t.Fatalf("system step: %v\n%s", err, out)
	}
	asound := env.read(t, "etc/asound.conf")
	if err := os.WriteFile(env.log, nil, 0o644); err != nil { // the user step's calls only
		t.Fatal(err)
	}
	home := t.TempDir()
	// Read-only for the session user: an /etc/asound.conf it may not change.
	if err := os.Chmod(filepath.Join(env.root, "etc"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(env.root, "etc"), 0o755) })
	out, err := env.runStep(t, AudioScript(true, true).Unprivileged, home)
	if err != nil || !strings.Contains(out, "virtual microphone ready") {
		t.Fatalf("user step: %v\n%s", err, out)
	}
	calls := env.calls(t)
	if strings.Contains(calls, "apt-get") || strings.Contains(calls, "sudo") {
		t.Fatalf("the user step installed or escalated:\n%s", calls)
	}
	if !strings.Contains(calls, "fleet output ensure") || !strings.Contains(calls, "fleet mic ensure") {
		t.Fatalf("the user step did not bring the session user's server up:\n%s", calls)
	}
	if env.read(t, "etc/asound.conf") != asound {
		t.Fatal("the user step changed /etc/asound.conf")
	}
	if _, err := os.Stat(filepath.Join(home, ".asoundrc")); err == nil {
		t.Fatal("the user step wrote the user's own config although the system's is there")
	}
}

// No root and no packages: the User step says so, with its own exit status,
// and installs nothing.
func TestAudioUserStepWithoutPackagesIsUnavailable(t *testing.T) {
	env := newMicScriptEnv(t)
	env.packageManager(t, "apt-get", env.audioInstallSnippet())
	out, err := env.runStep(t, AudioScript(true, true).Unprivileged, t.TempDir())
	var exit *exec.ExitError
	if err == nil || !errorAs(err, &exit) || exit.ExitCode() != AudioUnavailableExit {
		t.Fatalf("user step without packages: %v, want exit %d\n%s", err, AudioUnavailableExit, out)
	}
	if !strings.Contains(out, "no root to install them") {
		t.Fatalf("output: %s", out)
	}
	if strings.Contains(env.calls(t), "apt-get") {
		t.Fatal("the user step ran the package manager")
	}
}

// No root, packages in the image, nothing under /etc: the session user's own
// configuration routes ALSA and pulse clients to the private server.
func TestAudioUserStepWithoutRootWritesTheUsersOwnConfig(t *testing.T) {
	env := newMicScriptEnv(t)
	env.installAudio(t)
	env.stageFleet(t)
	home := t.TempDir()
	for run := 1; run <= 2; run++ {
		out, err := env.runStep(t, AudioScript(true, true).Unprivileged, home)
		if err != nil || !strings.Contains(out, "virtual microphone ready") {
			t.Fatalf("run %d: %v\n%s", run, err, out)
		}
	}
	if strings.Contains(env.calls(t), "sudo") || env.read(t, "etc/asound.conf") != "" || env.read(t, "etc/pulse/client.conf.d/00-fleet-mic.conf") != "" {
		t.Fatalf("the user step escalated or wrote /etc:\n%s", env.calls(t))
	}
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(home, rel))
		return string(data)
	}
	if asound := read(".asoundrc"); strings.Count(asound, "pcm.!default { type pulse }") != 1 || !strings.Contains(asound, micConfigMarker) {
		t.Fatalf("~/.asoundrc = %q", asound)
	}
	if drop := read(micsink.UserClientConfig); !strings.Contains(drop, "default-server = unix:"+micsink.SocketPath) {
		t.Fatalf("~/%s = %q", micsink.UserClientConfig, drop)
	}
	// PulseAudio reads ~/.config/pulse/client.conf.d only beside a
	// ~/.config/pulse/client.conf, which keeps the system-wide settings.
	if conf := read(".config/pulse/client.conf"); !strings.Contains(conf, ".include /etc/pulse/client.conf") {
		t.Fatalf("~/.config/pulse/client.conf = %q", conf)
	}

	// A user's own client.conf is kept as it is.
	own := "# mine\ndefault-sample-rate = 48000\n"
	if err := os.WriteFile(filepath.Join(home, ".config/pulse/client.conf"), []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := env.runStep(t, AudioScript(true, true).Unprivileged, home); err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	if read(".config/pulse/client.conf") != own {
		t.Fatal("the user's own client.conf was replaced")
	}
}

func TestAudioInstalledScript(t *testing.T) {
	env := newMicScriptEnv(t)
	if _, err := env.runStep(t, AudioInstalledScript(), t.TempDir()); err == nil {
		t.Fatal("no audio stack, but the check passed")
	}
	env.installAudio(t)
	if out, err := env.runStep(t, AudioInstalledScript(), t.TempDir()); err != nil {
		t.Fatalf("an installed stack failed the check: %v\n%s", err, out)
	}
}

func TestAudioScriptSteps(t *testing.T) {
	script := AudioScript(true, false)
	for name, body := range map[string]string{"all": script.Body, "system": script.Privileged, "user": script.Unprivileged} {
		if !strings.Contains(body, "step='"+name+"'") {
			t.Errorf("the %s body runs another step", name)
		}
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(wrap(Script{Name: "mic", Body: body}))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", name, err, out)
		}
	}
}

// rootFake is a backend with a root class of its own: it records whom each
// command ran as and runs it on the host.
type rootFake struct {
	backend.Backend
	capable  bool
	mu       sync.Mutex
	ran      []string // "root" / "user", in order
	commands []string // each command's argv, joined
	fail     string   // whom a command fails as
}

func (f *rootFake) record(who string, command []string) *backend.Cmd {
	f.mu.Lock()
	f.ran = append(f.ran, who)
	f.commands = append(f.commands, strings.Join(command, " "))
	fail := f.fail == who
	f.mu.Unlock()
	if fail {
		return backend.NewCmd(exec.Command("sh", "-c", "echo 'refused as "+who+"'; exit 7"), nil)
	}
	return backend.NewCmd(exec.Command("true"), nil)
}

func (f *rootFake) ExecCommand(_ string, command []string) *backend.Cmd {
	return f.record("user", command)
}
func (f *rootFake) RootCapable() bool { return f.capable }
func (f *rootFake) RootExecCommand(_ string, command []string) *backend.Cmd {
	return f.record("root", command)
}

// A split script on a backend with a root class runs its privileged half as
// root, then the rest as the user; without root only the rest; a failed
// privileged half stops it. A whole script, or a backend without a root
// class, runs Body as the user.
func TestRunPutsPrivilegedStepsOnTheRootClass(t *testing.T) {
	split := Script{Name: "audio", Body: "all", Privileged: "system", Unprivileged: "user"}
	for _, tc := range []struct {
		name    string
		b       *rootFake
		script  Script
		want    string
		failing bool
	}{
		{"root", &rootFake{capable: true}, split, "root,user", false},
		{"no root", &rootFake{capable: false}, split, "user", false},
		{"root fails", &rootFake{capable: true, fail: "root"}, split, "root", true},
		{"user fails", &rootFake{capable: true, fail: "user"}, split, "root,user", true},
		{"unsplit", &rootFake{capable: true}, Script{Name: "plain", Body: "x"}, "user", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := Run(tc.b, "/ws", []Script{tc.script})
			if got := strings.Join(tc.b.ran, ","); got != tc.want {
				t.Fatalf("ran as %s, want %s", got, tc.want)
			}
			if (len(errs) > 0) != tc.failing {
				t.Fatalf("errors %v", errs)
			}
			if tc.failing && tc.b.fail == "user" {
				if errs[0].LogPath != "~/.fleet/startup/audio.log" || strings.Contains(errs[0].Error(), "as root") {
					t.Fatalf("a failed user step: %v (log %q), want the user's audio.log", errs[0], errs[0].LogPath)
				}
				return
			}
			if tc.failing && !strings.Contains(errs[0].Error(), "as root") {
				t.Fatalf("the failure does not say it ran as root: %v", errs[0])
			}
			if tc.failing && errs[0].LogPath != "~root/.fleet/startup/audio-root.log" {
				t.Fatalf("a failed root step names log %q, want root's own audio-root.log", errs[0].LogPath)
			}
		})
	}
}

// A root step's log directory is under root's own home, not a HOME the
// image's environment sets for every exec (the session user's home, whose
// ~/.fleet must not become root's).
func TestRootStepLogsUnderRootsHome(t *testing.T) {
	b := &rootFake{capable: true}
	Run(b, "/ws", []Script{{Name: "audio", Body: "all", Privileged: "system", Unprivileged: "user"}})
	if len(b.commands) != 2 || !strings.Contains(b.commands[0], rootHome+"mkdir -p "+logDir) {
		t.Fatalf("the root step does not set root's home before its log directory: %q", b.commands)
	}
	if strings.Contains(b.commands[1], rootHome) {
		t.Fatal("the user step's home was changed")
	}
	userHome := filepath.Join(t.TempDir(), "home", "vscode")
	cmd := exec.Command("sh", "-c", rootHome+`printf %s "$HOME"`)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + userHome}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if home := string(out); home == userHome || !filepath.IsAbs(home) {
		t.Fatalf("HOME %q for the root step, want root's own", home)
	}
}

func errorAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
