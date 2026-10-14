package cli

import (
	"errors"
	"os/exec"
	"testing"
)

// stubExitRemoteShell records the code `fleet shell` would exit with.
func stubExitRemoteShell(t *testing.T) *int {
	t.Helper()
	got := -1
	orig := exitRemoteShell
	exitRemoteShell = func(code int) { got = code }
	t.Cleanup(func() { exitRemoteShell = orig })
	return &got
}

// A remote attach whose pod was replaced ends with exit 75 and the exit
// frame's "connection lost" error: `fleet shell` exits 75 (the wrappers'
// rejoin signal), not main's generic 1. Other ends are errors as before.
func TestRemoteShellResult(t *testing.T) {
	exited := stubExitRemoteShell(t)
	_ = remoteShellResult(kubeRejoinExitCode, errors.New("connection lost: the instance's pod was replaced"))
	if *exited != kubeRejoinExitCode {
		t.Fatalf("a replaced pod: exited %d, want %d", *exited, kubeRejoinExitCode)
	}
	*exited = -1
	if err := remoteShellResult(3, nil); err == nil || err.Error() != "exit status 3" || *exited != -1 {
		t.Fatalf("exit 3: %v (exited %d)", err, *exited)
	}
	if err := remoteShellResult(-1, errors.New("stream broke")); err == nil || *exited != -1 {
		t.Fatalf("a broken stream: %v (exited %d)", err, *exited)
	}
	if err := remoteShellResult(0, nil); err != nil || *exited != -1 {
		t.Fatalf("exit 0: %v (exited %d)", err, *exited)
	}
}

// A local attach (the kube-exec shim run in place) exiting 75 makes `fleet
// shell` exit 75 too; another exit code stays the *exec.ExitError main turns
// into 1.
func TestRejoinLocalExit(t *testing.T) {
	exited := stubExitRemoteShell(t)
	_ = rejoinLocalExit(exec.Command("sh", "-c", "exit 75").Run())
	if *exited != kubeRejoinExitCode {
		t.Fatalf("a local exit 75: exited %d, want %d", *exited, kubeRejoinExitCode)
	}
	*exited = -1
	err := rejoinLocalExit(exec.Command("sh", "-c", "exit 4").Run())
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 4 || *exited != -1 {
		t.Fatalf("a local exit 4: %v (exited %d)", err, *exited)
	}
	if err := rejoinLocalExit(nil); err != nil || *exited != -1 {
		t.Fatalf("success: %v (exited %d)", err, *exited)
	}
}
