package tui

import (
	"os/exec"
	"strings"
	"testing"
)

// runSh runs script under /bin/sh and returns its combined output.
func runSh(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	return string(out)
}

// rejoinScript re-runs an attach while it exits 75 (a replaced pod) and
// leaves the final code in $__rc; any other exit breaks the loop.
func TestRejoinScript(t *testing.T) {
	s := rejoinScript("fleet kube-exec --pod p -- tmux")
	if !strings.Contains(s, "fleet kube-exec --pod p -- tmux") {
		t.Fatalf("rejoin script dropped the command: %q", s)
	}
	if !strings.Contains(s, "= 75 ]") {
		t.Fatalf("rejoin script does not loop on exit 75: %q", s)
	}
	// A fake attach that exits 75 twice then 0: the loop runs three times
	// and ends with 0.
	script := "n=0; attach() { n=$((n+1)); [ $n -lt 3 ] && return 75 || return 0; }; " +
		rejoinScript("attach") + "; echo \"ran=$n rc=$__rc\""
	if out := strings.TrimSpace(runSh(t, script)); out != "ran=3 rc=0" {
		t.Fatalf("rejoin loop = %q, want ran=3 rc=0", out)
	}
	// A non-75 exit ends the loop at once with that code.
	script = "attach() { return 7; }; " + rejoinScript("attach") + "; echo \"rc=$__rc\""
	if out := strings.TrimSpace(runSh(t, script)); out != "rc=7" {
		t.Fatalf("a non-75 exit = %q, want rc=7", out)
	}
}
