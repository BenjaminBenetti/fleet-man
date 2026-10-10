//go:build linux

package execas

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probeHome is a home whose ~/.profile counts its runs and exports a variable
// and a PATH entry, as a user's startup files do.
func probeHome(t *testing.T, profile string) string {
	t.Helper()
	home := t.TempDir()
	if profile == "" {
		profile = `echo run >> "$HOME/runs"
export PROBED_VAR=hello
export PATH="$HOME/bin:$PATH"
`
	}
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func probeTarget(home string) ProbeTarget {
	me := CurrentCreds()
	return ProbeTarget{
		Mode:     ProbeLoginInteractiveShell,
		Shell:    "/bin/sh",
		Identity: Identity{UID: me.UID, GID: me.GID, Groups: me.Groups, Name: "me", Home: home, Shell: "/bin/sh"},
		Env:      []string{"HOME=" + home, "PATH=/usr/bin:/bin", "ENV="},
		Dir:      home,
	}
}

// probeRuns is how many times the probe shell ran.
func probeRuns(t *testing.T, home string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(lines(string(data)))
}

func TestProberReadsTheLoginEnvironment(t *testing.T) {
	home := probeHome(t, "")
	env := NewProber("").ProbeEnv(context.Background(), probeTarget(home))
	if env["PROBED_VAR"] != "hello" {
		t.Fatalf("PROBED_VAR = %q in %v", env["PROBED_VAR"], env)
	}
	if !strings.HasPrefix(env["PATH"], home+"/bin:") {
		t.Errorf("PATH = %q, want ~/bin first", env["PATH"])
	}
	if env["HOME"] != home {
		t.Errorf("HOME = %q", env["HOME"])
	}
	if _, ok := env["PWD"]; ok {
		t.Error("PWD survived the probe")
	}
}

func TestProberCachesUntilAStartupFileChanges(t *testing.T) {
	home := probeHome(t, "")
	p := NewProber("")
	target := probeTarget(home)
	ctx := context.Background()
	p.ProbeEnv(ctx, target)
	p.ProbeEnv(ctx, target)
	if n := probeRuns(t, home); n != 1 {
		t.Fatalf("probe ran %d times for an unchanged home, want 1", n)
	}

	// Editing ~/.profile changes its size and times: probe again.
	f, err := os.OpenFile(filepath.Join(home, ".profile"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("export ADDED_LATER=1\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	env := p.ProbeEnv(ctx, target)
	if n := probeRuns(t, home); n != 2 || env["ADDED_LATER"] != "1" {
		t.Fatalf("after an edit: %d runs, ADDED_LATER=%q", n, env["ADDED_LATER"])
	}

	// A new file in a drop-in directory counts too, as would a changed shell.
	drop := filepath.Join(home, ".config", "fish", "conf.d")
	if err := os.MkdirAll(drop, 0o755); err != nil {
		t.Fatal(err)
	}
	p.ProbeEnv(ctx, target)
	if n := probeRuns(t, home); n != 3 {
		t.Fatalf("a new drop-in directory: %d runs, want 3", n)
	}
	if err := os.WriteFile(filepath.Join(drop, "x.fish"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p.ProbeEnv(ctx, target)
	if n := probeRuns(t, home); n != 4 {
		t.Fatalf("a new drop-in file: %d runs, want 4", n)
	}

	// Another user, or the same user from another environment, is its own entry.
	other := target
	other.Env = append(other.Env, "LANG=C")
	p.ProbeEnv(ctx, other)
	if n := probeRuns(t, home); n != 5 {
		t.Fatalf("a different starting env: %d runs, want 5", n)
	}
}

func TestProberCacheExpires(t *testing.T) {
	home := probeHome(t, "")
	p := NewProber("")
	now := time.Now()
	p.now = func() time.Time { return now }
	target := probeTarget(home)
	p.ProbeEnv(context.Background(), target)
	now = now.Add(probeMaxAge - time.Second)
	p.ProbeEnv(context.Background(), target)
	if n := probeRuns(t, home); n != 1 {
		t.Fatalf("%d runs before the cache expired", n)
	}
	now = now.Add(2 * time.Second)
	p.ProbeEnv(context.Background(), target)
	if n := probeRuns(t, home); n != 2 {
		t.Fatalf("%d runs after the cache expired, want 2", n)
	}
}

func TestProberSharesItsCacheOnDisk(t *testing.T) {
	home := probeHome(t, "")
	cache := filepath.Join(t.TempDir(), "execas")
	target := probeTarget(home)
	first := NewProber(cache).ProbeEnv(context.Background(), target)

	fi, err := os.Stat(cache)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v, %v", fi, err)
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "probe-") {
		t.Fatalf("cache holds %v", entries)
	}
	if info, _ := entries[0].Info(); info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode %v", info.Mode())
	}

	// A fresh process (a one-shot exec-as) finds it without probing.
	second := NewProber(cache).ProbeEnv(context.Background(), target)
	if n := probeRuns(t, home); n != 1 || second["PROBED_VAR"] != first["PROBED_VAR"] {
		t.Fatalf("%d runs; second probe %v", n, second)
	}
}

func TestProberIgnoresACacheDirOthersCanWrite(t *testing.T) {
	home := probeHome(t, "")
	cache := filepath.Join(t.TempDir(), "execas")
	target := probeTarget(home)
	NewProber(cache).ProbeEnv(context.Background(), target)

	// Someone loosened it (or planted a lookalike): it is not trusted, so a
	// planted entry cannot hand a command its environment.
	if err := os.Chmod(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	NewProber(cache).ProbeEnv(context.Background(), target)
	if n := probeRuns(t, home); n != 2 {
		t.Fatalf("%d runs: a world-writable cache was trusted", n)
	}

	// A symlink to the real cache is followed to it, and judged by it.
	if err := os.Chmod(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(cache, link); err != nil {
		t.Fatal(err)
	}
	NewProber(link).ProbeEnv(context.Background(), target)
	if n := probeRuns(t, home); n != 2 {
		t.Fatalf("%d runs: a link to a private cache dir should serve it", n)
	}
}

func TestProberGivesUpOnAHangingShellAndKillsIt(t *testing.T) {
	home := probeHome(t, `sleep 300 &
echo $! > "$HOME/sleeper"
wait
`)
	p := NewProber("")
	p.timeout = 300 * time.Millisecond
	start := time.Now()
	if env := p.ProbeEnv(context.Background(), probeTarget(home)); env != nil {
		t.Fatalf("a timed-out probe returned %v", env)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not bound the probe")
	}
	data, err := os.ReadFile(filepath.Join(home, "sleeper"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid == 0 || !waitGone(pid, 3*time.Second) {
		t.Fatalf("the probe's background job (pid %d) outlived it", pid)
	}
	// A failed probe is not cached: the next call tries again.
	p.timeout = probeTimeout
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("export OK=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if env := p.ProbeEnv(context.Background(), probeTarget(home)); env["OK"] != "1" {
		t.Fatalf("after the fix: %v", env)
	}
}

func TestProberHonorsItsContext(t *testing.T) {
	home := probeHome(t, "sleep 300\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if env := NewProber("").ProbeEnv(ctx, probeTarget(home)); env != nil {
		t.Fatalf("cancelled probe returned %v", env)
	}
}

func TestProberFallsBackToPrintenv(t *testing.T) {
	home := probeHome(t, "")
	// A shell whose /proc dump prints nothing (as on a system without
	// /proc/self/environ) but which runs everything else.
	shell := filepath.Join(t.TempDir(), "noproc-sh")
	if err := os.WriteFile(shell, []byte(`#!/bin/sh
case "$2" in
*/proc/self/environ*) exit 0 ;;
esac
exec /bin/sh "$1" "$2"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	target := probeTarget(home)
	target.Shell = shell
	env := NewProber("").ProbeEnv(context.Background(), target)
	if env["PROBED_VAR"] != "hello" {
		t.Fatalf("printenv fallback: %v", env)
	}
}

func TestProberGetsNothingFromABrokenShell(t *testing.T) {
	home := probeHome(t, "")
	for _, shell := range []string{"/bin/false", "/no/such/shell", "/etc/passwd"} {
		target := probeTarget(home)
		target.Shell = shell
		if env := NewProber("").ProbeEnv(context.Background(), target); env != nil {
			t.Errorf("shell %s: %v", shell, env)
		}
	}
}

func TestProberRunsOneProbeForConcurrentCallers(t *testing.T) {
	p := NewProber("")
	var runs atomic.Int32
	release := make(chan struct{})
	p.run = func(context.Context, ProbeTarget) (map[string]string, bool) {
		runs.Add(1)
		<-release
		return map[string]string{"A": "1"}, true
	}
	target := probeTarget(t.TempDir())
	var wg sync.WaitGroup
	results := make([]map[string]string, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.ProbeEnv(context.Background(), target)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("%d probes for one target, want 1", runs.Load())
	}
	for _, r := range results {
		if r["A"] != "1" {
			t.Fatalf("a caller got %v", r)
		}
	}
	results[0]["A"] = "mutated"
	if p.ProbeEnv(context.Background(), target)["A"] != "1" {
		t.Fatal("callers share the cached map")
	}
}

func TestProberWaiterProbesAgainWhenTheLeaderIsCancelled(t *testing.T) {
	p := NewProber("")
	var runs atomic.Int32
	started := make(chan struct{})
	p.run = func(ctx context.Context, _ ProbeTarget) (map[string]string, bool) {
		if runs.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, false
		}
		return map[string]string{"A": "1"}, true
	}
	target := probeTarget(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	leader := make(chan map[string]string, 1)
	go func() { leader <- p.ProbeEnv(ctx, target) }()
	<-started
	waiter := make(chan map[string]string, 1)
	go func() { waiter <- p.ProbeEnv(context.Background(), target) }()
	time.Sleep(100 * time.Millisecond) // the waiter now waits on the leader's probe
	cancel()
	if env := <-leader; env != nil {
		t.Fatalf("the cancelled caller got %v", env)
	}
	if env := <-waiter; env["A"] != "1" {
		t.Fatalf("a caller whose context is live got %v from a probe another caller cancelled", env)
	}
	if runs.Load() != 2 {
		t.Fatalf("%d probes, want the cancelled one and the waiter's own", runs.Load())
	}
}

func TestProberMemoryCacheIsBounded(t *testing.T) {
	p := NewProber("")
	p.run = func(context.Context, ProbeTarget) (map[string]string, bool) { return map[string]string{}, true }
	base := probeTarget(t.TempDir())
	for i := range probeMemMax + 10 {
		target := base
		target.Env = append([]string{"N=" + strconv.Itoa(i)}, base.Env...)
		p.ProbeEnv(context.Background(), target)
	}
	if len(p.mem) != probeMemMax {
		t.Fatalf("%d entries cached, want the cap %d", len(p.mem), probeMemMax)
	}
}
