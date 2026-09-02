package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestOpenCommandCarriesTarget confirms the platform opener receives the target
// as its own argument (never shell-interpolated) on every supported OS.
func TestOpenCommandCarriesTarget(t *testing.T) {
	const target = "/tmp/chart with spaces.png"
	cmd, err := OpenCommand(target)
	if err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			t.Fatalf("unexpected error on %s: %v", runtime.GOOS, err)
		}
		return
	}
	carried := false
	for _, a := range cmd.Args {
		if a == target {
			carried = true
		}
	}
	for _, e := range cmd.Env {
		if strings.HasSuffix(e, "="+target) {
			carried = true
		}
	}
	if !carried {
		t.Errorf("opener should carry the target verbatim: args=%v", cmd.Args)
	}
}

// TestOpenFileCommandOverride covers FLEET_OPENER: the program (plus its own
// arguments) replaces the platform opener and the path is appended last.
func TestOpenFileCommandOverride(t *testing.T) {
	t.Setenv(EnvOpener, "  my-viewer --fullscreen ")
	cmd, err := OpenFileCommand("/tmp/x.png")
	if err != nil {
		t.Fatalf("OpenFileCommand: %v", err)
	}
	want := []string{"my-viewer", "--fullscreen", "/tmp/x.png"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestIsLauncherFile pins what OpenFile refuses: an exec-bit regular file and
// any launcher extension (file or directory), while a plain viewable file and a
// plain directory pass.
func TestIsLauncherFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bundle := filepath.Join(dir, "Thing.app")
	if err := os.Mkdir(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{mk("chart.png", 0o644), false},
		{mk("report.PDF", 0o644), false},
		{dir, false}, // a plain directory opens in the file manager
		{mk("tool", 0o755), true},
		{mk("run.SH", 0o644), true},
		{mk("thing.desktop", 0o644), true},
		{mk("setup.exe", 0o644), true},
		{bundle, true},
	}
	for _, tc := range cases {
		got, err := IsLauncherFile(tc.path)
		if err != nil {
			t.Fatalf("IsLauncherFile(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("IsLauncherFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if _, err := IsLauncherFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file should error")
	}
}

// TestOpenFileRefusesExecutable confirms OpenFile never invokes an opener for a
// launcher file, reports where the file is, and returns the absolute path.
func TestOpenFileRefusesExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec bits")
	}
	// Resolve the temp dir: on macOS it sits under a symlinked /var/folders,
	// and the cwd-based Abs below reports the real /private/var path.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "opened.log")
	stub := filepath.Join(dir, "opener.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$1\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOpener, stub)
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := OpenFile(tool)
	if err == nil || !strings.Contains(err.Error(), "refusing to open an executable") || !strings.Contains(err.Error(), tool) {
		t.Fatalf("executable should be refused naming the path, got %v", err)
	}
	if got != tool {
		t.Errorf("returned path = %q, want %q", got, tool)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the opener must not be invoked for a refused file")
	}

	// A viewable file goes through, with the absolute path.
	png := filepath.Join(dir, "chart.png")
	if err := os.WriteFile(png, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err = OpenFile("chart.png")
	if err != nil {
		t.Fatalf("OpenFile(chart.png): %v", err)
	}
	if got != png {
		t.Errorf("OpenFile should return the absolute path, got %q", got)
	}
	if opened, _ := os.ReadFile(log); strings.TrimSpace(string(opened)) != png {
		t.Errorf("opener should receive the absolute path, got %q", opened)
	}
}

// TestRunDetached covers the three outcomes: a prompt clean exit, a prompt
// failure (reported, with stderr), and a long-lived child that is left running
// after the grace period without holding the caller — and that no stderr temp
// file is left behind in any case.
func TestRunDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	t.Setenv("TMPDIR", t.TempDir())
	if err := runDetached(exec.Command("sh", "-c", "exit 0"), time.Second); err != nil {
		t.Errorf("clean exit: unexpected error %v", err)
	}
	err := runDetached(exec.Command("sh", "-c", "echo no handler >&2; exit 3"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "no handler") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("failure should report the exit status and stderr, got %v", err)
	}
	start := time.Now()
	if err := runDetached(exec.Command("sh", "-c", "sleep 5"), 100*time.Millisecond); err != nil {
		t.Errorf("long-lived child: unexpected error %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("long-lived child held the caller for %v", elapsed)
	}
	if _, err := exec.LookPath("no-such-opener-xyz"); err == nil {
		t.Skip("improbable: no-such-opener-xyz exists")
	}
	if err := runDetached(exec.Command("no-such-opener-xyz", "/tmp/x"), time.Second); err == nil {
		t.Errorf("missing program should fail to start")
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "fleet-open-*.log")); len(left) > 0 {
		t.Errorf("stderr temp files left behind: %v", left)
	}
}
