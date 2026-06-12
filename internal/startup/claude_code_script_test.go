package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInstaller mimics the behavior of claude.ai/install.sh that matters
// to the install script: it downloads to the hardcoded $HOME/.claude/downloads
// scratch dir, places the versioned binary under $HOME/.local/share/claude,
// and writes the $HOME/.local/bin/claude launcher symlink with an absolute
// target derived from the HOME it ran under. Keeping those three behaviors
// faithful is what lets these tests lock the off-the-shared-mount and
// symlink-fixup contracts.
const fakeInstaller = `#!/bin/bash
set -e
DOWNLOAD_DIR="$HOME/.claude/downloads"
mkdir -p "$DOWNLOAD_DIR"
echo binary-bytes > "$DOWNLOAD_DIR/claude-1.0.0-linux-x64"
versions="$HOME/.local/share/claude/versions"
mkdir -p "$versions"
printf '#!/bin/sh\necho "1.0.0 (Claude Code)"\n' > "$versions/1.0.0"
chmod +x "$versions/1.0.0"
mkdir -p "$HOME/.local/bin"
ln -sf "$versions/1.0.0" "$HOME/.local/bin/claude"
rm -f "$DOWNLOAD_DIR/claude-1.0.0-linux-x64"
`

// claudeScriptEnv is the sandbox a single test run of the claude-code
// script body executes in: an isolated HOME, an isolated TMPDIR (so the
// throwaway install home is observable), and a stub bin dir that shadows
// curl and sleep on PATH.
type claudeScriptEnv struct {
	home    string
	tmpDir  string
	stubBin string
	counter string
}

// newClaudeScriptEnv builds the sandbox. failures is the number of leading
// `curl | bash` attempts that must fail: the stub curl emits an installer
// that exits 1 ("Download failed", the symptom from issue #149) for the
// first `failures` calls and the faithful fake installer afterwards. The
// stub counts every call in a counter file so tests can assert attempts.
func newClaudeScriptEnv(t *testing.T, failures string) *claudeScriptEnv {
	t.Helper()
	env := &claudeScriptEnv{
		home:    t.TempDir(),
		tmpDir:  t.TempDir(),
		stubBin: t.TempDir(),
	}
	env.counter = filepath.Join(env.stubBin, "curl-calls")

	writeStub(t, env.stubBin, "curl", `#!/bin/sh
count=$(cat "`+env.counter+`" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "`+env.counter+`"
if [ "$count" -le "`+failures+`" ]; then
  echo 'echo "Download failed" >&2; exit 1'
  exit 0
fi
cat <<'INSTALLER'
`+fakeInstaller+`INSTALLER
`)
	// Stub sleep so the retry backoff doesn't slow the test down.
	writeStub(t, env.stubBin, "sleep", "#!/bin/sh\nexit 0\n")
	return env
}

// run executes the claude-code script body under sh with the sandbox
// environment. PATH deliberately excludes the host's /usr/local/bin etc.
// so a claude binary on the developer's machine can't short-circuit the
// install path under test.
func (env *claudeScriptEnv) run(t *testing.T) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available on this host: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available on this host: %v", err)
	}
	cmd := exec.Command(sh, "-c", claudeCodeScript().Body)
	cmd.Env = []string{
		"PATH=" + env.stubBin + ":/usr/bin:/bin",
		"HOME=" + env.home,
		"TMPDIR=" + env.tmpDir,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// curlCalls reports how many times the stub curl was invoked.
func (env *claudeScriptEnv) curlCalls(t *testing.T) string {
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

// writeStub drops an executable shell stub into dir.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write %s stub: %v", name, err)
	}
}

// TestClaudeCodeScript_InstallStaysOffSharedMount is the regression test
// for issue #149: the installer's download scratch dir must never land in
// the real ~/.claude (the fleet's shared mount), while the binary must
// still end up runnable at the real ~/.local/bin/claude — including after
// the throwaway install home is cleaned up.
func TestClaudeCodeScript_InstallStaysOffSharedMount(t *testing.T) {
	env := newClaudeScriptEnv(t, "0")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "claude installed: 1.0.0") {
		t.Errorf("output missing install confirmation:\n%s", out)
	}

	// The shared mount (real ~/.claude) must be untouched by the install.
	if _, err := os.Stat(filepath.Join(env.home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("install wrote to the real ~/.claude (the shared mount): stat err = %v", err)
	}

	// The launcher symlink must point into the real home, not the
	// (now removed) throwaway install home.
	launcher := filepath.Join(env.home, ".local", "bin", "claude")
	target, err := os.Readlink(launcher)
	if err != nil {
		t.Fatalf("readlink launcher: %v", err)
	}
	if !strings.HasPrefix(target, env.home+string(os.PathSeparator)) {
		t.Errorf("launcher target = %q, want a path under the real home %q", target, env.home)
	}
	version, err := exec.Command(launcher, "--version").Output()
	if err != nil {
		t.Fatalf("installed claude --version failed: %v", err)
	}
	if got := strings.TrimSpace(string(version)); got != "1.0.0 (Claude Code)" {
		t.Errorf("claude --version = %q, want %q", got, "1.0.0 (Claude Code)")
	}

	// The trap must have removed the throwaway install home.
	leftovers, err := os.ReadDir(env.tmpDir)
	if err != nil {
		t.Fatalf("read tmpdir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("throwaway install home not cleaned up: %v", leftovers)
	}
}

// TestClaudeCodeScript_RetriesTransientFailure simulates the concurrent
// clobbering from issue #149 (installer exits 1 with "Download failed")
// on the first two attempts and asserts the retry loop self-heals.
func TestClaudeCodeScript_RetriesTransientFailure(t *testing.T) {
	env := newClaudeScriptEnv(t, "2")

	out, err := env.run(t)
	if err != nil {
		t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
	}
	if got := env.curlCalls(t); got != "3" {
		t.Errorf("curl invoked %s times, want 3 (two failures + one success)", got)
	}
	if _, err := os.Stat(filepath.Join(env.home, ".local", "bin", "claude")); err != nil {
		t.Errorf("launcher missing after retried install: %v", err)
	}
}

// TestClaudeCodeScript_GivesUpAfterThreeAttempts asserts the retry loop is
// bounded and surfaces a non-zero exit (which the runner reports as a
// warning) once all attempts are exhausted.
func TestClaudeCodeScript_GivesUpAfterThreeAttempts(t *testing.T) {
	env := newClaudeScriptEnv(t, "99")

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
	// Even on failure the throwaway install home must be cleaned up.
	leftovers, err := os.ReadDir(env.tmpDir)
	if err != nil {
		t.Fatalf("read tmpdir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("throwaway install home not cleaned up: %v", leftovers)
	}
}

// TestClaudeCodeScript_SkipsWhenAlreadyInstalled asserts the idempotency
// short-circuit: with a claude already on PATH the script must not invoke
// the installer at all.
func TestClaudeCodeScript_SkipsWhenAlreadyInstalled(t *testing.T) {
	env := newClaudeScriptEnv(t, "0")
	writeStub(t, env.stubBin, "claude", "#!/bin/sh\necho \"9.9.9 (Claude Code)\"\n")

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
