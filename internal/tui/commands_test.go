package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// trickyWords are argv words a shell would split, expand or run if they were
// joined unquoted: spaces (paths, kube contexts), quotes, expansions and
// command separators.
var trickyWords = []string{
	"/opt/fleet app/fleet",
	"arn:aws:eks:us-east-1:123456789012:cluster/prod team",
	"it's",
	"$HOME",
	"`id`",
	"a;b",
	"",
}

// TestLogsCommandQuotesArgvAndLogPath runs the logs view's pager script and
// checks the server-resolved runtime-logs argv reaches the command word for
// word, and that the creation log is read from a HOME the shell would
// otherwise expand or split.
func TestLogsCommandQuotesArgvAndLogPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "my $HOME `x` it's")
	t.Setenv("HOME", home)
	logPath := filepath.Join(fleetpaths.Dir(), "logs", "alpha-agent-1.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("creation output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := resolveLogsArgv
	resolveLogsArgv = func(fleetName, instanceName string) ([]string, error) {
		return append([]string{"printf", "[%s]\\n"}, trickyWords...), nil
	}
	defer func() { resolveLogsArgv = prev }()

	cmd := logsCommand("alpha", &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning})
	cmd.Stdin = strings.NewReader("\n") // answers the "press Enter" prompt
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("logs script failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "creation output\n") {
		t.Errorf("creation log not shown:\n%s", got)
	}
	var want strings.Builder
	for _, word := range trickyWords {
		want.WriteString("[" + word + "]\n")
	}
	if !strings.Contains(got, want.String()) {
		t.Errorf("runtime-logs argv not passed word for word; got:\n%s\nwant it to contain:\n%s", got, want.String())
	}
}

// TestFleetShellCmdQuotesEveryWord runs the command line a rebound split key
// and a group restore hand to the shell, with a fleet binary whose path has
// spaces and a quote, and checks the binary gets exactly its arguments.
func TestFleetShellCmdQuotesEveryWord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Fleet App", "it's")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "fleet")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nprintf '[%s]\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		flags []string
		want  string
	}{
		{"no group", nil, "[shell]\n[alpha/agent-1]\n"},
		{"group", []string{"--group", "g~1"}, "[shell]\n[alpha/agent-1]\n[--group]\n[g~1]\n"},
		{"session", []string{"--session", "agent-1~g $x"}, "[shell]\n[alpha/agent-1]\n[--session]\n[agent-1~g $x]\n"},
	}
	// A plain binary path stays a bare command word: the bound split keys run
	// this line in the user's default-shell, which may not be POSIX.
	if got, want := fleetShellCmd("/usr/local/bin/fleet", "alpha/agent-1"), "/usr/local/bin/fleet 'shell' 'alpha/agent-1'"; got != want {
		t.Errorf("fleetShellCmd with a plain path = %q, want %q", got, want)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("sh", "-c", fleetShellCmd(self, "alpha/agent-1", tc.flags...)).CombinedOutput()
			if err != nil {
				t.Fatalf("sh -c %q: %v\n%s", fleetShellCmd(self, "alpha/agent-1", tc.flags...), err, out)
			}
			if string(out) != tc.want {
				t.Errorf("got\n%s\nwant\n%s", out, tc.want)
			}
		})
	}
}

// TestDaemonLogStreamCommandQuotesTheLogPath runs the daemon log stream's
// script with tail stubbed out and checks fleet.log's path reaches tail as one
// word under a HOME the shell would otherwise expand.
func TestDaemonLogStreamCommandQuotesTheLogPath(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "my $HOME `x` \"q\""))
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tail"), []byte("#!/bin/sh\nprintf '[%s]' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := daemonLogStreamCommand(daemonLogLevels[0])
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("log stream script failed: %v\n%s", err, out)
	}
	if want := "[-n][200][-F][" + filepath.Join(fleetpaths.Dir(), "fleet.log") + "]"; !strings.Contains(string(out), want) {
		t.Errorf("tail got\n%s\nwant it to contain\n%s", out, want)
	}
}
