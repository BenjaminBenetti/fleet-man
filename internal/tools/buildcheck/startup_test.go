package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// helperEnv makes the test binary act as a stand-in fleet: it exits at once,
// after a 40ms sleep when it runs under the name "slow".
const helperEnv = "BUILDCHECK_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if filepath.Base(os.Args[0]) == "slow" {
			time.Sleep(40 * time.Millisecond)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helpers copies the test binary to dir/fast and dir/slow.
func helpers(t *testing.T) (fast, slow string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fast, slow = filepath.Join(dir, "fast"), filepath.Join(dir, "slow")
	for _, p := range []string{fast, slow} {
		if err := os.WriteFile(p, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return fast, slow
}

func TestMeasure(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	fast, slow := helpers(t)
	env, err := benchEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, helperEnv+"=1")

	same, err := measure(benchCase{name: "same", base: fast, head: fast, args: []string{"version"}, budget: 15 * time.Millisecond}, 7, 1, env)
	if err != nil {
		t.Fatal(err)
	}
	if same.baseWall <= 0 || same.headWall <= 0 {
		t.Errorf("medians not recorded: %+v", same)
	}
	if p := same.problems(); len(p) != 0 {
		t.Errorf("a binary against itself broke the budget: %v", p)
	}
	if runtime.GOOS == "linux" && (same.baseRSS <= 0 || same.headRSS <= 0) {
		t.Errorf("RSS not recorded: base %d head %d", same.baseRSS, same.headRSS)
	}

	grew, err := measure(benchCase{name: "grew", base: fast, head: slow, args: []string{"version"}, budget: 15 * time.Millisecond}, 5, 1, env)
	if err != nil {
		t.Fatal(err)
	}
	if grew.delta() < 30*time.Millisecond {
		t.Errorf("delta %s, want about 40ms", grew.delta())
	}
	if p := grew.problems(); len(p) != 1 || !strings.Contains(p[0], "grew: start-up grew") {
		t.Errorf("problems = %v", p)
	}
}

func TestMeasureReportsAFailingCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	fast, _ := helpers(t)
	env, err := benchEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Without the helper variable the test binary runs its tests and fails
	// on the unknown flag.
	_, err = measure(benchCase{name: "broken", base: fast, head: fast, args: []string{"-no-such-flag"}}, 1, 0, env)
	if err == nil || !strings.Contains(err.Error(), "broken:") {
		t.Errorf("err = %v, want the case's failure", err)
	}
}

func TestProblems(t *testing.T) {
	r := benchResult{
		benchCase: benchCase{name: "pod: version", budget: 15 * time.Millisecond, maxRSS: 40 << 20},
		baseWall:  3 * time.Millisecond,
		headWall:  18 * time.Millisecond,
		headRSS:   20 << 20,
	}
	if p := r.problems(); len(p) != 0 {
		t.Errorf("exactly at the budget: %v", p)
	}
	r.headWall = 19 * time.Millisecond
	r.headRSS = 41 << 20
	p := r.problems()
	if len(p) != 2 || !strings.Contains(p[0], "start-up grew 16.0ms (3.0ms -> 19.0ms), budget 15.0ms") || !strings.Contains(p[1], "RSS 41.0 MiB, budget 40.0 MiB") {
		t.Errorf("problems = %q", p)
	}
	r.maxRSS = 0
	if p := r.problems(); len(p) != 1 {
		t.Errorf("no RSS ceiling still flagged RSS: %q", p)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]time.Duration{5, 1, 3}); got != 3 {
		t.Errorf("odd median = %d", got)
	}
	if got := median([]int64{4, 1, 3, 2}); got != 2 {
		t.Errorf("even median = %d", got)
	}
	if got := median([]int64(nil)); got != 0 {
		t.Errorf("empty median = %d", got)
	}
}

func TestStartupReport(t *testing.T) {
	dir := t.TempDir()
	base, head := filepath.Join(dir, "base"), filepath.Join(dir, "head")
	if err := os.WriteFile(base, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(head, make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	results := []benchResult{
		{benchCase: benchCase{name: "pod: version", budget: 15 * time.Millisecond, maxRSS: 40 << 20},
			baseWall: 3 * time.Millisecond, headWall: 4 * time.Millisecond, baseRSS: 20 << 20, headRSS: 21 << 20},
		{benchCase: benchCase{name: "host: __complete", budget: 15 * time.Millisecond},
			baseWall: 3 * time.Millisecond, headWall: 30 * time.Millisecond, baseRSS: -1, headRSS: -1},
	}
	got := startupReport("origin/main", 41, results, [][3]string{{"full", base, head}, {"pod", base, filepath.Join(dir, "missing")}})
	for _, want := range []string{
		"### Start-up budget vs origin/main",
		"Median of 41 interleaved runs",
		"| pod: version | 3.0ms | 4.0ms | +1.0ms | 15.0ms | 20.0 MiB | 21.0 MiB | 40.0 MiB | ok |",
		"| host: __complete | 3.0ms | 30.0ms | +27.0ms | 15.0ms | n/a | n/a | - | **over budget** |",
		"| full | 1.0 MiB | 3.0 MiB | +2.0 MiB |",
		"| pod | 1.0 MiB | n/a | - |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}

func TestRunStartupFlags(t *testing.T) {
	if err := runStartup([]string{"-full", "x"}, io.Discard); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Errorf("missing binaries: err = %v", err)
	}
	if err := runStartup([]string{"-full", "a", "-base-full", "b", "-pod", "c", "-base-pod", "d", "-runs", "0"}, io.Discard); err == nil {
		t.Error("-runs 0 accepted")
	}
}

func TestRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("no args: code %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"bogus"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("unknown command: code %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "startup [flags]") {
		t.Errorf("help: code %d, stdout %q", code, stdout.String())
	}
	stderr.Reset()
	if code := run([]string{"static"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "buildcheck static: no files to check") {
		t.Errorf("failing check: code %d, stderr %q", code, stderr.String())
	}
}
