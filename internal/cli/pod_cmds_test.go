package cli

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
)

func TestPodCommandsAreHiddenAndTakeNoArgs(t *testing.T) {
	root := NewRootCmd()
	for path, flags := range map[string]map[string]string{
		"pod-init":          {"spec": podinit.DefaultSpecPath},
		"pod-job":           {"spec": "/fleet-tools/job.json"},
		"pod-job feed":      {"socket": "/fleet-tools/job.sock", "dial-timeout": jobboot.DefaultDialTimeout.String()},
		"pod-job ws-export": {"dir": ""},
		"pod-job statfs":    {"path": "/opt/fleet"},
		"pod-layout":        {"spec": "/opt/fleet/spec/layout.json", "tree": "/fleet-share"},
	} {
		cmd, _, err := root.Find(splitWords(path))
		if err != nil || cmd.CommandPath() != "fleet "+path {
			t.Fatalf("%s not registered: %v", path, err)
		}
		if !strings.Contains(path, " ") && !cmd.Hidden {
			t.Errorf("%s is internal and must stay out of fleet --help", path)
		}
		if cmd.Args(cmd, []string{"x"}) == nil {
			t.Errorf("%s takes no arguments", path)
		}
		for flag, want := range flags {
			f := cmd.Flags().Lookup(flag)
			if f == nil || f.DefValue != want {
				t.Errorf("%s --%s default = %v, want %q", path, flag, f, want)
			}
		}
	}
}

func splitWords(s string) []string {
	var out []string
	word := ""
	for _, r := range s {
		if r == ' ' {
			out, word = append(out, word), ""
			continue
		}
		word += string(r)
	}
	return append(out, word)
}

func TestPodInitAndPodJobExitWithTheirCodes(t *testing.T) {
	oldInit, oldJob := exitPodInit, exitPodJob
	defer func() { exitPodInit, exitPodJob = oldInit, oldJob }()
	var initCode, jobCode int
	exitPodInit = func(c int) { initCode = c }
	exitPodJob = func(c int) { jobCode = c }

	missing := filepath.Join(t.TempDir(), "missing.json")
	root := NewRootCmd()
	root.SetArgs([]string{"pod-init", "--spec", missing})
	_ = root.Execute()
	if initCode != execas.ExitFailed {
		t.Fatalf("pod-init with no spec exited %d, want %d", initCode, execas.ExitFailed)
	}
	root = NewRootCmd()
	root.SetArgs([]string{"pod-job", "--spec", missing})
	_ = root.Execute()
	// Off linux pod-job refuses before it reads any spec, and exits with its
	// plain failure (pod-init's refusal there is its plain failure too).
	want := jobboot.ExitBadSpec
	if runtime.GOOS != "linux" {
		want = jobboot.ExitFailed
	}
	if jobCode != want {
		t.Fatalf("pod-job with no spec exited %d, want %d", jobCode, want)
	}
}
