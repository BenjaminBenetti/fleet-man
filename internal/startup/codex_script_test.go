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
	home    string
	tmpDir  string
	stubBin string
	counter string
}

// newCodexScriptEnv builds the sandbox. failures is the number of leading
// curl download attempts that must fail: the stub curl exits 1 (a failed
// download) for the first `failures` calls and afterwards writes a
// faithful release tarball — a gzipped tar holding the single
// codex-<target> static binary — to curl's -o destination. The stub
// counts every call in a counter file so tests can assert attempts.
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
  if [ "$1" = "-o" ]; then out="$2"; shift; fi
  shift
done
if [ "$count" -le "`+failures+`" ]; then
  echo "curl: (23) Failure writing output to destination" >&2
  exit 1
fi
case "$(uname -m)" in
  x86_64 | amd64)  target="x86_64-unknown-linux-musl" ;;
  aarch64 | arm64) target="aarch64-unknown-linux-musl" ;;
esac
stage=$(mktemp -d)
printf '#!/bin/sh\necho "codex-cli 1.0.0"\n' > "$stage/codex-$target"
chmod +x "$stage/codex-$target"
tar -czf "$out" -C "$stage" "codex-$target"
rm -rf "$stage"
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
		"PATH=" + env.stubBin + ":/usr/bin:/bin",
		"HOME=" + env.home,
		"TMPDIR=" + env.tmpDir,
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

// TestCodexScript_InstallsStandaloneBinary is the regression test for
// issue #145: the install must depend on nothing beyond curl + tar (the
// sandbox PATH has no node/npm/gawk), must leave a runnable binary at
// the real ~/.local/bin/codex, must keep every write out of ~/.codex
// (the fleet's shared mount), and must wire ~/.local/bin into
// ~/.profile for login shells.
func TestCodexScript_InstallsStandaloneBinary(t *testing.T) {
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
