package backendtest

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
)

// caseStats: Stats reports the running instance, and an empty request is not
// an error. Usage is only required to be non-negative: a docker without
// memory accounting (nested in a container whose cgroup does not delegate the
// memory controller) reports 0.
func caseStats(c *C) {
	i := c.Shared()
	got, err := i.B.Stats([]string{i.ID()})
	if err != nil {
		c.Fatalf("Stats: %v", err)
	}
	s, ok := got[i.ID()]
	switch {
	case !ok || s == nil:
		c.Errorf("Stats has no entry for the running instance %s: %v", i.ID(), got)
	case s.MemoryMB < 0 || s.CPUMillicores < 0:
		c.Errorf("Stats for the running instance: %+v, want no negative usage", *s)
	default:
		c.Logf("Stats for the running instance: %.0f MB, %.1f millicores", s.MemoryMB, s.CPUMillicores)
	}
	if none, err := i.B.Stats(nil); err != nil || len(none) != 0 {
		c.Errorf("Stats(nil) = %v, %v; want nothing and no error", none, err)
	}
}

// caseLogs: without follow, LogsCommand and Logs return once the log is
// printed instead of streaming on.
func caseLogs(c *C) {
	i := c.Shared()
	if r := runCmd(i.B.LogsCommand(i.ID(), false), nil, c.within(2*time.Minute)); r.Err != nil {
		c.Errorf("LogsCommand without follow: %s", r)
	}
	done := make(chan error, 1)
	go func() { done <- i.B.Logs(i.ID(), false) }()
	select {
	case err := <-done:
		if err != nil {
			c.Errorf("Logs without follow: %v", err)
		}
	case <-time.After(c.within(2 * time.Minute)):
		c.Errorf("Logs without follow did not return in time")
	}
}

// caseSessions: a tmux session RunScript starts is seen by ListSessions and
// CaptureAllSessions (the session poller's reads) and by an ExecCommand (an
// attach), so all of them reach one tmux server; ListSessions stays within
// the target's latency budget.
func caseSessions(c *C) {
	i := c.Shared()
	if !c.NeedTool(i, "tmux") {
		return
	}
	name := "conformance-" + randHex(3)
	marker := "pane-" + randHex(4)
	c.Script(i, fmt.Sprintf("tmux new-session -d -s %s %s", name, shellquote.Single("echo "+marker+"; exec sleep 600")))
	c.Cleanup(func() { _, _ = i.B.RunScript(i.ID(), "tmux kill-session -t "+name) })

	var line string
	c.Eventually("ListSessions to list "+name, 15*time.Second, func() (bool, string) {
		out := i.B.ListSessions(i.ID())
		line = sessionLine(out, name)
		return line != "", out
	})
	if fields := strings.Split(line, ":"); len(fields) != 3 || fields[1] != "1" || fields[2] != "0" {
		c.Errorf("ListSessions line %q, want %s:1:0 (name:windows:attached)", line, name)
	}
	c.Eventually("CaptureAllSessions to capture the pane", 15*time.Second, func() (bool, string) {
		all := i.B.CaptureAllSessions(i.ID())
		capture, ok := all.Sessions[name]
		return all.OK && ok && strings.Contains(capture.Content, marker), fmt.Sprintf("%+v", all)
	})
	if r := c.Exec(i, []string{"tmux", "has-session", "-t", name}, nil); r.Err != nil {
		c.Errorf("ExecCommand cannot see the session RunScript started (a different user or tmux server): %s", r)
	}

	const samples = 20
	times := make([]time.Duration, samples)
	for n := range times {
		start := time.Now()
		_ = i.B.ListSessions(i.ID())
		times[n] = time.Since(start)
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	p95 := times[samples*95/100-1]
	c.Logf("ListSessions p95 over %d calls: %s (min %s, max %s)", samples, p95, times[0], times[samples-1])
	if budget := c.Target().ListSessionsP95; budget > 0 && p95 > budget {
		c.Errorf("ListSessions p95 %s is over the %s budget", p95, budget)
	}

	c.Script(i, "tmux kill-session -t "+name)
	c.Eventually("ListSessions to drop "+name, 15*time.Second, func() (bool, string) {
		out := i.B.ListSessions(i.ID())
		return sessionLine(out, name) == "", out
	})
}

// sessionLine is name's line in ListSessions output ("" when absent).
func sessionLine(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+":") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// caseToolProbe: AgentToolProbe finds nothing in a fresh instance, finds an
// agent while one runs, and stops finding it once it exits.
func caseToolProbe(c *C) {
	i := c.Shared()
	if c.Target().HostProcesses {
		c.Skipf("instance commands run on this host, where a process scan sees the host's own agents")
	}
	probe := func(want string) {
		c.Helper()
		c.Eventually(fmt.Sprintf("AgentToolProbe to report %q", want), 20*time.Second, func() (bool, string) {
			tool, ok := i.B.AgentToolProbe(i.ID())
			return ok && tool == want, fmt.Sprintf("tool %q ok %v", tool, ok)
		})
	}
	probe("")
	bin := shellquote.Single(c.Scratch(i) + "/bin")
	pid := c.Script(i, fmt.Sprintf(`mkdir -p %[1]s && cp "$(command -v sleep)" %[1]s/claude && %[1]s/claude 600 >/dev/null 2>&1 & echo $!`, bin))
	c.Cleanup(func() { _, _ = i.B.RunScript(i.ID(), "kill "+pid+" 2>/dev/null") })
	probe("claude")
	c.Script(i, "kill "+pid)
	probe("")
}

// caseEditorURI: an editor URI is a vscode-remote URI naming the instance and
// opening its remote workspace folder, the same every time.
func caseEditorURI(c *C) {
	i := c.Shared()
	project := filepath.Base(i.WorkspaceDir())
	uri, ok := i.B.EditorURI(i.WorkspaceDir(), project)
	if !ok {
		c.Skipf("%s has no editor integration", c.Target().Name)
	}
	u, err := url.Parse(uri)
	switch {
	case err != nil:
		c.Errorf("EditorURI %q does not parse: %v", uri, err)
	case u.Scheme != "vscode-remote" || u.Host == "":
		c.Errorf("EditorURI %q: want vscode-remote://<authority>/<folder>", uri)
	case u.Path != i.RemoteWorkspace():
		c.Errorf("EditorURI %q opens %q, want the remote workspace folder %q", uri, u.Path, i.RemoteWorkspace())
	}
	if again, _ := i.B.EditorURI(i.WorkspaceDir(), project); again != uri {
		c.Errorf("EditorURI changed between calls: %q then %q", uri, again)
	}
}
