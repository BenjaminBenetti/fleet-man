package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codexScriptEnv is the sandbox a single test run of the codex script
// body executes in: an isolated HOME, an isolated TMPDIR (so the
// download scratch dir is observable), and a stub bin dir that shadows
// curl and sleep on PATH.
type codexScriptEnv struct {
	home          string
	tmpDir        string
	stubBin       string
	counter       string
	installerMode string
}

// newCodexScriptEnv builds the sandbox. failures is the number of leading
// curl download attempts that must fail: the stub curl exits 1 (a failed
// download) for the first `failures` calls and afterwards writes a stub
// official installer to curl's -o destination. The installer models the
// complete package layout and checks the install-time environment.
func newCodexScriptEnv(t *testing.T, failures string) *codexScriptEnv {
	t.Helper()
	env := &codexScriptEnv{
		home:    t.TempDir(),
		tmpDir:  t.TempDir(),
		stubBin: t.TempDir(),
	}
	env.counter = filepath.Join(env.stubBin, "curl-calls")

	writeStub(t, env.stubBin, "curl", `#!/bin/sh
count=$(cat "`+env.counter+`" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "`+env.counter+`"
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    https://*) [ "$1" = "https://chatgpt.com/codex/install.sh" ] || exit 1 ;;
  esac
  if [ "$1" = "-o" ]; then out="$2"; shift; fi
  shift
done
if [ "$count" -le "`+failures+`" ]; then
  echo "curl: (23) Failure writing output to destination" >&2
  exit 1
fi
cp "`+filepath.Join(env.stubBin, "installer")+`" "$out"
`)
	writeStub(t, env.stubBin, "installer", `#!/bin/sh
set -eu
[ "$CODEX_NON_INTERACTIVE" = 1 ]
[ "$CODEX_INSTALL_DIR" = "$HOME/.local/bin" ]
[ "$CODEX_HOME" = "$HOME/.local/share/fleet/codex" ]
root="$CODEX_HOME/packages/standalone"
release="$root/releases/1.0.0-test"
mkdir -p "$release/bin" "$release/codex-path" "$release/codex-resources" "$CODEX_INSTALL_DIR"
printf '{}\n' > "$release/codex-package.json"
printf '#!/bin/sh\necho "codex-cli 1.0.0"\n' > "$release/bin/codex"
for resource in bin/codex-code-mode-host codex-path/rg codex-resources/bwrap; do
  printf '#!/bin/sh\nexit 0\n' > "$release/$resource"
  chmod +x "$release/$resource"
done
chmod +x "$release/bin/codex"
ln -sfn "$release" "$root/current"
ln -sf "$root/current/bin/codex" "$CODEX_INSTALL_DIR/codex"
case "$TEST_INSTALLER_MODE" in
  fail) exit 1 ;;
  missing-host) rm "$release/bin/codex-code-mode-host" ;;
  missing-rg) rm "$release/codex-path/rg" ;;
  missing-bwrap) rm "$release/codex-resources/bwrap" ;;
  broken-cli) printf '#!/bin/sh\nexit 1\n' > "$release/bin/codex" ;;
esac
`)
	// Stub sleep so the retry backoff doesn't slow the test down.
	writeStub(t, env.stubBin, "sleep", "#!/bin/sh\nexit 0\n")
	return env
}

// run executes the codex script body under sh with the sandbox
// environment. PATH deliberately excludes the host's /usr/local/bin etc.
// so a codex binary on the developer's machine can't short-circuit the
// install path under test.
func (env *codexScriptEnv) run(t *testing.T) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available on this host: %v", err)
	}
	cmd := exec.Command(sh, "-c", codexScript().Body)
	cmd.Env = []string{
		"PATH=" + env.stubBin + ":" + filepath.Join(env.home, ".local", "bin") + ":/usr/bin:/bin",
		"HOME=" + env.home,
		"TMPDIR=" + env.tmpDir,
		"CODEX_HOME=" + filepath.Join(env.home, ".codex"),
		"TEST_INSTALLER_MODE=" + env.installerMode,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// curlCalls reports how many times the stub curl was invoked.
func (env *codexScriptEnv) curlCalls(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(env.counter)
	if os.IsNotExist(err) {
		return "0"
	}
	if err != nil {
		t.Fatalf("read curl counter: %v", err)
	}
	return strings.TrimSpace(string(content))
}

// TestCodexScript_InstallsCompletePackage covers the missing Code Mode host
// regression and issue #145: the install must leave a runnable binary at
// the real ~/.local/bin/codex, must keep every write out of ~/.codex
// (the fleet's shared mount), and must wire ~/.local/bin into
// ~/.profile for login shells.
func TestCodexScript_InstallsCompletePackage(t *testing.T) {
	env := newCodexScriptEnv(t, "0")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "codex installed: codex-cli 1.0.0") {
		t.Errorf("output missing install confirmation:\n%s", out)
	}

	launcher := filepath.Join(env.home, ".local", "bin", "codex")
	version, err := exec.Command(launcher, "--version").Output()
	if err != nil {
		t.Fatalf("installed codex --version failed: %v", err)
	}
	if got := strings.TrimSpace(string(version)); got != "codex-cli 1.0.0" {
		t.Errorf("codex --version = %q, want %q", got, "codex-cli 1.0.0")
	}
	resolved, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		t.Fatalf("resolve installed launcher: %v", err)
	}
	for _, resource := range []string{"bin/codex-code-mode-host", "codex-path/rg", "codex-resources/bwrap"} {
		path := filepath.Join(filepath.Dir(filepath.Dir(resolved)), resource)
		if err := exec.Command(path, "--help").Run(); err != nil {
			t.Errorf("bundled resource %s is not runnable: %v", resource, err)
		}
	}

	// The shared mount (real ~/.codex) must be untouched by the install.
	if _, err := os.Stat(filepath.Join(env.home, ".codex")); !os.IsNotExist(err) {
		t.Errorf("install wrote to ~/.codex (the shared mount): stat err = %v", err)
	}

	// Login shells must find ~/.local/bin.
	profile, err := os.ReadFile(filepath.Join(env.home, ".profile"))
	if err != nil {
		t.Fatalf("read ~/.profile: %v", err)
	}
	if !strings.Contains(string(profile), `.local/bin`) {
		t.Errorf("~/.profile not wired for ~/.local/bin:\n%s", profile)
	}
	if strings.Contains(string(profile), "CODEX_HOME") {
		t.Errorf("installer changed runtime Codex home: %s", profile)
	}

	// The trap must have removed the download scratch dir.
	leftovers, err := os.ReadDir(env.tmpDir)
	if err != nil {
		t.Fatalf("read tmpdir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("download scratch dir not cleaned up: %v", leftovers)
	}
}

// TestCodexScript_SkipsProfileAppendWhenAlreadyWired asserts the
// ~/.profile guard: a profile that already references .local/bin (the
// stock Debian snippet, or a previous install) must not gain a
// duplicate PATH line.
func TestCodexScript_SkipsProfileAppendWhenAlreadyWired(t *testing.T) {
	env := newCodexScriptEnv(t, "0")
	stock := "PATH=\"$HOME/.local/bin:$PATH\"\n"
	if err := os.WriteFile(filepath.Join(env.home, ".profile"), []byte(stock), 0o644); err != nil {
		t.Fatalf("seed ~/.profile: %v", err)
	}

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	profile, err := os.ReadFile(filepath.Join(env.home, ".profile"))
	if err != nil {
		t.Fatalf("read ~/.profile: %v", err)
	}
	if string(profile) != stock {
		t.Errorf("~/.profile modified despite existing .local/bin entry:\n%s", profile)
	}
}

// TestCodexScript_RetriesTransientFailure simulates a transient download
// failure on the first two attempts and asserts the retry loop self-heals.
func TestCodexScript_RetriesTransientFailure(t *testing.T) {
	env := newCodexScriptEnv(t, "2")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	if got := env.curlCalls(t); got != "3" {
		t.Errorf("curl invoked %s times, want 3 (two failures + one success)", got)
	}
	if _, err := os.Stat(filepath.Join(env.home, ".local", "bin", "codex")); err != nil {
		t.Errorf("binary missing after retried install: %v", err)
	}
}

// TestCodexScript_GivesUpAfterThreeAttempts asserts the retry loop is
// bounded and surfaces a non-zero exit (which the runner reports as a
// warning) once all attempts are exhausted.
func TestCodexScript_GivesUpAfterThreeAttempts(t *testing.T) {
	env := newCodexScriptEnv(t, "99")

	out, err := env.run(t)
	if err == nil {
		t.Fatalf("script exited zero despite persistent install failure\noutput: %s", out)
	}
	if !strings.Contains(out, "failed after 3 attempts") {
		t.Errorf("output missing give-up message:\n%s", out)
	}
	if got := env.curlCalls(t); got != "3" {
		t.Errorf("curl invoked %s times, want exactly 3", got)
	}
	// Even on failure the download scratch dir must be cleaned up.
	leftovers, err := os.ReadDir(env.tmpDir)
	if err != nil {
		t.Fatalf("read tmpdir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("download scratch dir not cleaned up: %v", leftovers)
	}
}

// TestCodexScript_SkipsWhenAlreadyInstalled asserts the idempotency
// short-circuit: with a codex already on PATH the script must not
// download anything at all.
func TestCodexScript_SkipsWhenAlreadyInstalled(t *testing.T) {
	env := newCodexScriptEnv(t, "0")
	writeStub(t, env.stubBin, "codex", "#!/bin/sh\necho \"codex-cli 9.9.9\"\n")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "already installed") {
		t.Errorf("output missing already-installed short-circuit:\n%s", out)
	}
	if got := env.curlCalls(t); got != "0" {
		t.Errorf("curl invoked %s times, want 0", got)
	}
}

func TestCodexScript_RepairsLegacyInstall(t *testing.T) {
	env := newCodexScriptEnv(t, "0")
	bin := filepath.Join(env.home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, bin, "codex", "#!/bin/sh\necho 'codex-cli 0.1.0'\n")
	out, err := env.run(t)
	if err != nil || !strings.Contains(out, "codex installed: codex-cli 1.0.0") {
		t.Fatalf("legacy install was not repaired: %v\n%s", err, out)
	}
}

func TestCodexScript_SkipsCompleteFleetInstall(t *testing.T) {
	env := newCodexScriptEnv(t, "0")
	if out, err := env.run(t); err != nil {
		t.Fatalf("initial install failed: %v\n%s", err, out)
	}
	out, err := env.run(t)
	if err != nil || !strings.Contains(out, "already installed") {
		t.Fatalf("complete install was not skipped: %v\n%s", err, out)
	}
	if got := env.curlCalls(t); got != "1" {
		t.Errorf("curl invoked %s times, want only the initial download", got)
	}
}

func TestCodexScript_RejectsIncompleteInstall(t *testing.T) {
	for _, mode := range []string{"fail", "missing-host", "missing-rg", "missing-bwrap", "broken-cli"} {
		t.Run(mode, func(t *testing.T) {
			env := newCodexScriptEnv(t, "0")
			env.installerMode = mode
			out, err := env.run(t)
			if err == nil || !strings.Contains(out, "failed after 3 attempts") {
				t.Fatalf("incomplete install reported success: %v\n%s", err, out)
			}
			if got := env.curlCalls(t); got != "3" {
				t.Errorf("curl invoked %s times, want 3", got)
			}
			env.installerMode = ""
			out, err = env.run(t)
			// A nonzero installer can still leave a complete package; all
			// missing-resource cases must actually reinstall on the next run.
			if err != nil || (mode != "fail" && !strings.Contains(out, "codex installed:")) {
				t.Fatalf("subsequent startup did not repair install: %v\n%s", err, out)
			}
		})
	}
}

func TestCodexScript_DoesNotExecuteFailedDownload(t *testing.T) {
	env := newCodexScriptEnv(t, "0")
	writeStub(t, env.stubBin, "curl", `#!/bin/sh
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    printf 'touch "$HOME/partial-installer-executed"\n' > "$2"
    break
  fi
  shift
done
exit 1
`)
	if out, err := env.run(t); err == nil {
		t.Fatalf("failed download reported success: %s", out)
	}
	if _, err := os.Stat(filepath.Join(env.home, "partial-installer-executed")); !os.IsNotExist(err) {
		t.Fatalf("partial installer was executed: %v", err)
	}
}
