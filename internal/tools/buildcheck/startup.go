package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Budgets from the k8s plan: the pod build's start-up may grow at most 15ms
// over the base build, and so may the host build's `fleet version` and
// `fleet __complete` (the shell runs it on every Tab press). The pod build's
// resident set stays under the in-pod agent's 40MB.
const (
	defaultStartupBudget = 15 * time.Millisecond
	defaultPodMaxRSSMiB  = 40
)

// benchCase is one command timed on the base and head binaries.
type benchCase struct {
	name       string
	base, head string
	args       []string
	budget     time.Duration // largest allowed head-minus-base median
	maxRSS     int64         // head median RSS ceiling in bytes; 0 for none
}

// benchResult holds a case's medians. RSS is -1 where the OS does not report it.
type benchResult struct {
	benchCase
	baseWall, headWall time.Duration
	baseRSS, headRSS   int64
}

func (r benchResult) delta() time.Duration { return r.headWall - r.baseWall }

// problems lists the budgets the result breaks.
func (r benchResult) problems() []string {
	var p []string
	if r.delta() > r.budget {
		p = append(p, fmt.Sprintf("%s: start-up grew %s (%s -> %s), budget %s",
			r.name, ms(r.delta()), ms(r.baseWall), ms(r.headWall), ms(r.budget)))
	}
	if r.maxRSS > 0 && r.headRSS > r.maxRSS {
		p = append(p, fmt.Sprintf("%s: RSS %s, budget %s", r.name, mib(r.headRSS), mib(r.maxRSS)))
	}
	return p
}

func runStartup(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("startup", flag.ContinueOnError)
	baseFull := fs.String("base-full", "", "full build of the base ref")
	headFull := fs.String("full", "", "full build under test")
	basePod := fs.String("base-pod", "", "pod build (-tags nok8s) of the base ref")
	headPod := fs.String("pod", "", "pod build under test")
	runs := fs.Int("runs", 41, "timed runs per binary and case")
	warmup := fs.Int("warmup", 3, "untimed runs per binary and case first")
	budget := fs.Duration("budget", defaultStartupBudget, "largest allowed start-up growth per case")
	podMaxRSS := fs.Int64("pod-max-rss-mib", defaultPodMaxRSSMiB, "pod build RSS ceiling in MiB (0 for none)")
	baseLabel := fs.String("base-label", "base", "name of the base ref in the report")
	summary := fs.String("summary", "", "also append the markdown report to this file (e.g. $GITHUB_STEP_SUMMARY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, f := range []struct{ name, value string }{
		{"base-full", *baseFull}, {"full", *headFull}, {"base-pod", *basePod}, {"pod", *headPod},
	} {
		if f.value == "" {
			return fmt.Errorf("-%s is required", f.name)
		}
	}
	if *runs < 1 {
		return errors.New("-runs must be at least 1")
	}

	cases := []benchCase{
		{name: "pod: version", base: *basePod, head: *headPod, args: []string{"version"},
			budget: *budget, maxRSS: *podMaxRSS << 20},
		{name: "host: version", base: *baseFull, head: *headFull, args: []string{"version"}, budget: *budget},
		{name: "host: __complete", base: *baseFull, head: *headFull, args: []string{"__complete", ""}, budget: *budget},
	}

	home, err := os.MkdirTemp("", "buildcheck-startup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	env, err := benchEnv(home)
	if err != nil {
		return err
	}

	var results []benchResult
	for _, c := range cases {
		r, err := measure(c, *runs, *warmup, env)
		if err != nil {
			return err
		}
		results = append(results, r)
	}
	sizes := [][3]string{{"full", *baseFull, *headFull}, {"pod", *basePod, *headPod}}
	report := startupReport(*baseLabel, *runs, results, sizes)

	fmt.Fprint(stdout, report)
	if *summary != "" {
		if err := appendFile(*summary, report); err != nil {
			return err
		}
	}
	var failed []string
	for _, r := range results {
		failed = append(failed, r.problems()...)
	}
	if len(failed) > 0 {
		return fmt.Errorf("over budget:\n  %s", strings.Join(failed, "\n  "))
	}
	return nil
}

// benchEnv is the environment every timed run gets: a throwaway HOME (so no
// command touches real fleet state or reaches a daemon) and the caller's PATH.
func benchEnv(home string) ([]string, error) {
	tmp := filepath.Join(home, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	return []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}, nil
}

// measure times the case's command on both binaries, alternating which goes
// first each round so drift and ordering effects hit both alike, and keeps the
// medians.
func measure(c benchCase, runs, warmup int, env []string) (benchResult, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return benchResult{}, err
	}
	defer devnull.Close()

	bins := [2]string{c.base, c.head}
	var walls [2][]time.Duration
	var rsss [2][]int64
	for i := 0; i < warmup+runs; i++ {
		order := [2]int{0, 1}
		if i%2 == 1 {
			order = [2]int{1, 0}
		}
		for _, side := range order {
			wall, rss, err := runOnce(bins[side], c.args, env, devnull)
			if err != nil {
				return benchResult{}, fmt.Errorf("%s: %w", c.name, err)
			}
			if i >= warmup {
				walls[side] = append(walls[side], wall)
				rsss[side] = append(rsss[side], rss)
			}
		}
	}
	return benchResult{
		benchCase: c,
		baseWall:  median(walls[0]),
		headWall:  median(walls[1]),
		baseRSS:   median(rsss[0]),
		headRSS:   median(rsss[1]),
	}, nil
}

// runOnce runs bin once, timing it from fork to reap.
func runOnce(bin string, args, env []string, devnull *os.File) (time.Duration, int64, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	if err != nil {
		return 0, 0, fmt.Errorf("%s %s: %w\n%s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return wall, maxRSS(cmd.ProcessState), nil
}

func median[T time.Duration | int64](xs []T) T {
	if len(xs) == 0 {
		return 0
	}
	s := append([]T(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// startupReport renders the results as markdown, for the log and the CI job
// summary. sizes rows are {name, base binary, head binary}.
func startupReport(baseLabel string, runs int, results []benchResult, sizes [][3]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Start-up budget vs %s\n\n", baseLabel)
	fmt.Fprintf(&b, "Median of %d interleaved runs per binary.\n\n", runs)
	b.WriteString("| case | base | head | delta | budget | base RSS | head RSS | RSS budget | result |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		verdict := "ok"
		if len(r.problems()) > 0 {
			verdict = "**over budget**"
		}
		rssBudget := "-"
		if r.maxRSS > 0 {
			rssBudget = mib(r.maxRSS)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.name, ms(r.baseWall), ms(r.headWall), signedMs(r.delta()), ms(r.budget),
			mib(r.baseRSS), mib(r.headRSS), rssBudget, verdict)
	}
	b.WriteString("\n| binary | base size | head size | delta |\n|---|---|---|---|\n")
	for _, s := range sizes {
		base, head := fileSize(s[1]), fileSize(s[2])
		delta := "-"
		if base >= 0 && head >= 0 {
			delta = fmt.Sprintf("%+.1f MiB", float64(head-base)/(1<<20))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", s[0], mib(base), mib(head), delta)
	}
	b.WriteString("\n")
	return b.String()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond)) }

func signedMs(d time.Duration) string {
	return fmt.Sprintf("%+.1fms", float64(d)/float64(time.Millisecond))
}

func mib(n int64) string {
	if n < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
