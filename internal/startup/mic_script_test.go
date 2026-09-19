package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

// micScriptEnv is the sandbox one run of the mic script executes in: a fake
// filesystem root (FLEET_MIC_ROOT) standing in for /etc and the alsa-lib plugin
// dirs, and a PATH holding ONLY stubs plus the handful of real coreutils the
// script needs — so the host's own apt-get / pulseaudio can never leak in.
type micScriptEnv struct {
	root    string
	stubBin string
	log     string // every stub package-manager / sudo invocation, one per line
}

func newMicScriptEnv(t *testing.T) *micScriptEnv {
	t.Helper()
	env := &micScriptEnv{root: t.TempDir(), stubBin: t.TempDir()}
	env.log = filepath.Join(env.stubBin, "calls.log")
	for _, tool := range []string{"sh", "id", "grep", "dirname", "mkdir", "tee", "cat", "touch", "env", "true", "chmod"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available on this host: %v", tool, err)
		}
		if err := os.Symlink(real, filepath.Join(env.stubBin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	// Passwordless sudo that just runs the command (the test is not root).
	writeStub(t, env.stubBin, "sudo", `#!/bin/sh
echo "sudo $*" >> "`+env.log+`"
[ "$1" = -n ] && shift
exec "$@"
`)
	return env
}

// installAudio makes the sandbox look like the audio stack is installed.
func (env *micScriptEnv) installAudio(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pulseaudio", "pactl", "arecord"} {
		writeStub(t, env.stubBin, bin, "#!/bin/sh\nexit 0\n")
	}
	plugin := filepath.Join(env.root, "usr/lib/x86_64-linux-gnu/alsa-lib")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "libasound_module_pcm_pulse.so"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// packageManager adds a stub package manager whose "install" runs onInstall (a
// shell snippet) — typically materialising the audio stack.
func (env *micScriptEnv) packageManager(t *testing.T, name, onInstall string) {
	t.Helper()
	writeStub(t, env.stubBin, name, `#!/bin/sh
echo "`+name+` $*" >> "`+env.log+`"
case " $* " in *" install "*|*" add "*) `+onInstall+` ;; esac
exit 0
`)
}

// audioInstallSnippet is shell that does what installAudio does, for use as a
// package manager's onInstall.
func (env *micScriptEnv) audioInstallSnippet() string {
	plugin := filepath.Join(env.root, "usr/lib64/alsa-lib")
	return `for b in pulseaudio pactl arecord; do printf '#!/bin/sh\nexit 0\n' > "` + env.stubBin + `/$b"; chmod +x "` + env.stubBin + `/$b"; done; mkdir -p "` + plugin + `"; touch "` + plugin + `/libasound_module_pcm_pulse.so"`
}

func (env *micScriptEnv) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(env.stubBin, "sh"), "-c", MicScript().Body)
	cmd.Env = []string{"PATH=" + env.stubBin, "FLEET_MIC_ROOT=" + env.root}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (env *micScriptEnv) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.root, rel))
	if err != nil {
		return ""
	}
	return string(data)
}

func (env *micScriptEnv) calls(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(env.log)
	return string(data)
}

func (env *micScriptEnv) writeRoot(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(env.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Already-installed is the rebuild / re-run path: no package manager is touched,
// both config files are (re)written.
func TestMicScriptConfiguresAnInstalledStack(t *testing.T) {
	env := newMicScriptEnv(t)
	env.installAudio(t)
	env.packageManager(t, "apt-get", "exit 99")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(env.calls(t), "apt-get") {
		t.Fatalf("an installed stack must not invoke the package manager:\n%s", env.calls(t))
	}
	asound := env.read(t, "etc/asound.conf")
	if !strings.Contains(asound, "pcm.!default { type pulse }") || !strings.Contains(asound, micConfigMarker) {
		t.Fatalf("asound.conf = %q", asound)
	}
	client := env.read(t, "etc/pulse/client.conf.d/00-fleet-mic.conf")
	if !strings.Contains(client, "default-server = unix:"+micsink.SocketPath) || !strings.Contains(client, "autospawn = no") {
		t.Fatalf("client.conf drop-in = %q", client)
	}
	if !strings.Contains(out, "virtual microphone ready") {
		t.Fatalf("output: %s", out)
	}

	// Idempotent: a second run rewrites fleet's own (marked) file without fuss.
	if out, err := env.run(t); err != nil || strings.Contains(out, "leaving the existing") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
}

func TestMicScriptInstallsPerPackageManager(t *testing.T) {
	for manager, want := range map[string]string{
		"apt-get": "apt-get install -y -qq --no-install-recommends pulseaudio pulseaudio-utils libasound2-plugins alsa-utils",
		"apk":     "apk add --no-cache pulseaudio pulseaudio-utils alsa-plugins-pulse alsa-utils",
		"dnf":     "dnf install -y --allowerasing pulseaudio pulseaudio-utils alsa-plugins-pulseaudio alsa-utils",
	} {
		t.Run(manager, func(t *testing.T) {
			env := newMicScriptEnv(t)
			env.packageManager(t, manager, env.audioInstallSnippet())
			out, err := env.run(t)
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			if !strings.Contains(env.calls(t), want) {
				t.Fatalf("calls:\n%s\nwant %q", env.calls(t), want)
			}
			if !strings.Contains(env.calls(t), "sudo -n") {
				t.Fatal("a non-root install must go through passwordless sudo")
			}
			if env.read(t, "etc/asound.conf") == "" {
				t.Fatal("asound.conf not written after install")
			}
		})
	}
}

// RPM distros: alsa-lib SHIPS /etc/asound.conf, so fleet's own install creates
// the file. Judged after the install, fleet would refuse to touch it — forever.
func TestMicScriptOwnsAnAsoundConfItsInstallCreated(t *testing.T) {
	env := newMicScriptEnv(t)
	stock := filepath.Join(env.root, "etc/asound.conf")
	env.packageManager(t, "dnf", env.audioInstallSnippet()+
		`; mkdir -p "`+filepath.Dir(stock)+`"; echo "# stock alsa-lib config" > "`+stock+`"`)

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if asound := env.read(t, "etc/asound.conf"); !strings.Contains(asound, "pcm.!default { type pulse }") {
		t.Fatalf("the install-created asound.conf was left in place: %q\n%s", asound, out)
	}
}

// A pre-existing asound.conf is the image's. Leaving it alone is right — but if
// that leaves the ALSA default dead, saying "ready" would be a lie that ends in
// silent recordings.
func TestMicScriptReportsAForeignAsoundConfThatBypassesPulse(t *testing.T) {
	env := newMicScriptEnv(t)
	env.installAudio(t)
	env.writeRoot(t, "etc/asound.conf", "pcm.!default { type hw card 0 }\n")

	out, err := env.run(t)
	if err == nil {
		t.Fatalf("a dead ALSA default must fail the script, got success:\n%s", out)
	}
	if !strings.Contains(out, "WARNING") || strings.Contains(out, "virtual microphone ready") {
		t.Fatalf("output: %s", out)
	}
	if got := env.read(t, "etc/asound.conf"); got != "pcm.!default { type hw card 0 }\n" {
		t.Fatalf("the image's asound.conf was modified: %q", got)
	}
	// The pulse side is fleet's own and must be configured regardless.
	if env.read(t, "etc/pulse/client.conf.d/00-fleet-mic.conf") == "" {
		t.Fatal("client.conf drop-in missing")
	}
}

// …whereas a foreign asound.conf that already reaches PulseAudio (its own
// routing, or the pulse plugin's drop-in) is simply fine.
func TestMicScriptAcceptsAForeignAsoundConfThatReachesPulse(t *testing.T) {
	for name, setup := range map[string]func(*micScriptEnv, *testing.T){
		"routes itself": func(env *micScriptEnv, t *testing.T) {
			env.writeRoot(t, "etc/asound.conf", "pcm.!default { type pulse }\n")
		},
		"plugin drop-in": func(env *micScriptEnv, t *testing.T) {
			env.writeRoot(t, "etc/asound.conf", "# stock\n")
			env.writeRoot(t, "usr/share/alsa/alsa.conf.d/50-pulseaudio.conf", "pcm.!default { type pulse }\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newMicScriptEnv(t)
			env.installAudio(t)
			setup(env, t)
			out, err := env.run(t)
			if err != nil || !strings.Contains(out, "virtual microphone ready") {
				t.Fatalf("err=%v\n%s", err, out)
			}
			if !strings.Contains(out, "leaving the existing") {
				t.Fatalf("the foreign file should be reported as left alone:\n%s", out)
			}
		})
	}
}

func TestMicScriptWithoutAPackageManager(t *testing.T) {
	env := newMicScriptEnv(t)
	out, err := env.run(t)
	if err == nil || !strings.Contains(out, "no supported package manager") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

// An install that "succeeds" without delivering the tools is a failure, with
// the right diagnosis.
func TestMicScriptVerifiesTheInstall(t *testing.T) {
	env := newMicScriptEnv(t)
	env.packageManager(t, "apt-get", ":")
	out, err := env.run(t)
	if err == nil || !strings.Contains(out, "still missing after install") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}

func TestMicScriptShape(t *testing.T) {
	script := MicScript()
	if script.Name != "mic" {
		t.Fatalf("Name = %q", script.Name)
	}
	// The staged binary is addressed absolutely, like the sink itself: the exec
	// user's PATH is not to be relied on.
	if !strings.Contains(script.Body, "fleet_bin='/usr/bin/fleet'") || strings.Contains(script.Body, "command -v fleet") {
		t.Fatal("the script should run the staged fleet binary by absolute path")
	}
	if strings.Contains(script.Body, "find ") {
		t.Fatal("the script must not depend on findutils")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(wrap(script))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The microphone is a global setting: no FleetSettings toggle may pull it in.
func TestScriptsForNeverIncludesMic(t *testing.T) {
	all := fleet.FleetSettings{ClaudeCodeMount: true, CodexMount: true, AuggieMount: true}
	for _, script := range ScriptsFor(all) {
		if script.Name == "mic" {
			t.Fatal("ScriptsFor must not return the mic script")
		}
	}
}
