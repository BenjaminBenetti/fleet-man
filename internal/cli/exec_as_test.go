package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

func TestExecAsIsHiddenAndTakesNoArgs(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"exec-as"})
	if err != nil || cmd.Name() != "exec-as" {
		t.Fatalf("exec-as not registered: %v", err)
	}
	if !cmd.Hidden {
		t.Fatal("exec-as is internal and must stay out of fleet --help")
	}
	if cmd.Args(cmd, []string{"bash"}) == nil {
		t.Fatal("exec-as must take its request on stdin, never in argv")
	}
	for flag, want := range map[string]string{"spec": execas.DefaultSpecPath, "cache-dir": execas.DefaultCacheDir} {
		f := cmd.Flags().Lookup(flag)
		if f == nil || f.DefValue != want {
			t.Errorf("--%s default = %v, want %q", flag, f, want)
		}
	}
}

func TestExecAsExitsWithTheFailuresCode(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("not a frame\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin, oldExit := os.Stdin, exitExecAs
	defer func() { os.Stdin, exitExecAs = oldStdin, oldExit }()
	os.Stdin = r
	code := -1
	exitExecAs = func(c int) { code = c }

	root := NewRootCmd()
	root.SetArgs([]string{"exec-as", "--spec", "/nonexistent/exec.json"})
	err = root.Execute()
	if code != execas.ExitFailed || err == nil || !strings.Contains(err.Error(), "not an exec request frame") {
		t.Fatalf("code %d err %v, want exit %d for a bad frame", code, err, execas.ExitFailed)
	}
}
