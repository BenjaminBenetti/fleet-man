package execas

import (
	"errors"
	"strings"
	"testing"
)

func TestRequestValidate(t *testing.T) {
	ok := []Request{
		{Class: ClassExec, Argv: []string{"bash"}},
		{Class: ClassScript, Argv: []string{"sh", "-c", ""}},
		{Class: ClassRoot, Argv: []string{"apt-get"}, User: "root"},
		{Class: ClassRoot, Argv: []string{"apt-get"}, User: "0"},
		{Class: ClassExplicit, Argv: []string{"x"}, User: "1000:1000", Cwd: "/w", Env: map[string]string{"A": ""}},
		{Class: ClassExec, Argv: []string{"bash"}, TTY: true, Rows: 24, Cols: 80},
	}
	for _, req := range ok {
		if err := req.Validate(); err != nil {
			t.Errorf("%+v: %v", req, err)
		}
	}
	for _, tc := range []struct {
		req  Request
		want string
	}{
		{Request{Argv: []string{"x"}}, "unknown exec class"},
		{Request{Class: "sudo", Argv: []string{"x"}}, "unknown exec class"},
		{Request{Class: ClassExec}, "no command"},
		{Request{Class: ClassExec, Argv: []string{""}}, "no command"},
		{Request{Class: ClassExec, Argv: []string{"a", "b\x00"}}, "NUL"},
		{Request{Class: ClassExec, Argv: []string{"a"}, User: "a\x00"}, "NUL"},
		{Request{Class: ClassRoot, Argv: []string{"a"}, User: "vscode"}, "runs as root"},
		{Request{Class: ClassExec, Argv: []string{"a"}, Cwd: "relative"}, "not an absolute path"},
		{Request{Class: ClassExec, Argv: []string{"a"}, Env: map[string]string{"A=B": "x"}}, "contains '='"},
		{Request{Class: ClassExec, Argv: []string{"a"}, Rows: 24}, "needs tty"},
	} {
		if err := tc.req.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want one containing %q", tc.req, err, tc.want)
		}
	}
}

func TestClasses(t *testing.T) {
	for _, c := range Classes() {
		if !c.Valid() {
			t.Errorf("%q is listed but not valid", c)
		}
	}
	if len(Classes()) != 4 {
		t.Fatalf("Classes() = %v", Classes())
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Fatal("nil is exit 0")
	}
	if ExitCode(notFound(errNoCommand)) != 127 || ExitCode(cannotRun(errNoCommand)) != 126 || ExitCode(failed(errNoCommand)) != 125 {
		t.Fatal("ExitError codes")
	}
	if ExitCode(errNoCommand) != ExitFailed {
		t.Fatal("a plain error is exit 125")
	}
	if (ExitStatus{Code: 3}).ShellCode() != 3 || (ExitStatus{Code: -1, Signal: 9}).ShellCode() != 137 {
		t.Fatal("ShellCode")
	}
}

var errNoCommand = errors.New("no command")
