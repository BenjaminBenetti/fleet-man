package kubeshim

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseRunGrammar(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantFlags []Flag
		wantImage string
		wantCmd   []string
		wantErr   string
	}{
		{
			name:      "long options with a separate and an attached value",
			args:      []string{"--mount", "type=bind,source=/a,target=/b", "--sig-proxy=false", "img"},
			wantFlags: []Flag{{"mount", "type=bind,source=/a,target=/b"}, {"sig-proxy", "false"}},
			wantImage: "img",
		},
		{
			name:      "an attached value keeps every later =",
			args:      []string{"--env=K=V=W", "-l", "devcontainer.metadata=[{\"a\":\"=\"}]", "img"},
			wantFlags: []Flag{{"env", "K=V=W"}, {"label", "devcontainer.metadata=[{\"a\":\"=\"}]"}},
			wantImage: "img",
		},
		{
			name:      "bundled booleans",
			args:      []string{"-dit", "img"},
			wantFlags: []Flag{{"detach", "true"}, {"interactive", "true"}, {"tty", "true"}},
			wantImage: "img",
		},
		{
			name:      "a bundle ending in a shorthand that takes the next argument",
			args:      []string{"-itu", "root", "img"},
			wantFlags: []Flag{{"interactive", "true"}, {"tty", "true"}, {"user", "root"}},
			wantImage: "img",
		},
		{
			name:      "shorthand values attached, after =, and separate",
			args:      []string{"-eKEY=VALUE", "-u=root", "-p", "8080:80", "-p8081:81", "img"},
			wantFlags: []Flag{{"env", "KEY=VALUE"}, {"user", "root"}, {"publish", "8080:80"}, {"publish", "8081:81"}},
			wantImage: "img",
		},
		{
			name:      "a boolean shorthand given a value",
			args:      []string{"-d=false", "img"},
			wantFlags: []Flag{{"detach", "false"}},
			wantImage: "img",
		},
		{
			name:      "hidden aliases report the canonical name",
			args:      []string{"--net=host", "--net-alias", "a", "--dns-opt", "ndots:1", "img"},
			wantFlags: []Flag{{"network", "host"}, {"network-alias", "a"}, {"dns-option", "ndots:1"}},
			wantImage: "img",
		},
		{
			name:      "everything after the image is the command, options included",
			args:      []string{"--init", "img", "-c", "echo started", "--rm", "-"},
			wantFlags: []Flag{{"init", "true"}},
			wantImage: "img",
			wantCmd:   []string{"-c", "echo started", "--rm", "-"},
		},
		{
			name:      "-- ends the options",
			args:      []string{"--rm", "--", "-img-", "x"},
			wantFlags: []Flag{{"rm", "true"}},
			wantImage: "-img-",
			wantCmd:   []string{"x"},
		},
		{name: "an unknown long option", args: []string{"--frobnicate", "img"}, wantErr: "unknown flag: --frobnicate"},
		{name: "an unknown shorthand", args: []string{"-Z", "img"}, wantErr: "unknown flag: -Z"},
		{name: "a missing value", args: []string{"--entrypoint"}, wantErr: "flag needs an argument"},
		{name: "a missing shorthand value", args: []string{"-u"}, wantErr: "flag needs an argument"},
		{name: "a bad boolean", args: []string{"--rm=maybe", "img"}, wantErr: "invalid boolean"},
		{name: "no image", args: []string{"--rm"}, wantErr: ErrNoImage.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := ParseRun(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(run.Flags, tt.wantFlags) {
				t.Errorf("flags = %v, want %v", run.Flags, tt.wantFlags)
			}
			if run.Image != tt.wantImage {
				t.Errorf("image = %q, want %q", run.Image, tt.wantImage)
			}
			if len(run.Cmd) != 0 || len(tt.wantCmd) != 0 {
				if !reflect.DeepEqual(run.Cmd, tt.wantCmd) {
					t.Errorf("cmd = %q, want %q", run.Cmd, tt.wantCmd)
				}
			}
		})
	}
}

// TestParseRunOfTheCLIsRun parses a docker run exactly as devcontainer CLI
// 0.89.0 emits it.
func TestParseRunOfTheCLIsRun(t *testing.T) {
	args := []string{
		"--sig-proxy=false", "-a", "STDOUT", "-a", "STDERR",
		"--mount", "type=bind,source=/ws/app,target=/workspaces/app",
		"-l", "devcontainer.local_folder=/ws/app",
		"-l", "devcontainer.config_file=/ws/app/.devcontainer/devcontainer.json",
		"-e", "KEY=a value",
		"--init", "--cap-add", "SYS_PTRACE", "--security-opt", "seccomp=unconfined",
		"--entrypoint", "/bin/sh",
		"-l", `devcontainer.metadata=[{"remoteUser":"vscode"}]`,
		"mcr.microsoft.com/devcontainers/base:debian-12",
		"-c", "echo Container started\ntrap \"exit 0\" 15\n\nexec \"$@\"\nwhile sleep 1 & wait $!; do :; done", "-",
	}
	run, err := ParseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	if run.Image != "mcr.microsoft.com/devcontainers/base:debian-12" || len(run.Cmd) != 3 || run.Cmd[2] != "-" {
		t.Fatalf("image %q cmd %q", run.Image, run.Cmd)
	}
	if got := run.Values("attach"); !reflect.DeepEqual(got, []string{"STDOUT", "STDERR"}) {
		t.Errorf("attach = %v", got)
	}
	if ep, _ := run.Value("entrypoint"); ep != "/bin/sh" {
		t.Errorf("entrypoint = %q", ep)
	}
	if run.Bool("sig-proxy", true) || !run.Bool("init", false) || run.Bool("privileged", false) {
		t.Errorf("booleans: sig-proxy %v init %v privileged %v", run.Bool("sig-proxy", true), run.Bool("init", false), run.Bool("privileged", false))
	}
	want := []string{"attach", "cap-add", "entrypoint", "env", "init", "label", "mount", "security-opt", "sig-proxy"}
	if got := run.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestUnknownFlagIsTheUnsupportedFlagError(t *testing.T) {
	_, err := ParseRun([]string{"--frobnicate=1", "img"})
	var unknown *UnknownFlagError
	if !errors.As(err, &unknown) || unknown.Flag != "--frobnicate" {
		t.Fatalf("err = %v", err)
	}
	got := runParseError(err).Error()
	if !strings.HasPrefix(got, "kubernetes:unsupported:flag:frobnicate: ") {
		t.Errorf("runParseError = %q", got)
	}
	if UnsupportedFlag("-Z") != "kubernetes:unsupported:flag:Z" {
		t.Errorf("UnsupportedFlag(-Z) = %q", UnsupportedFlag("-Z"))
	}
}

func TestRunFlagNamesAreCanonicalAndSorted(t *testing.T) {
	names := RunFlagNames()
	seen := map[string]bool{}
	for i, n := range names {
		if seen[n] {
			t.Errorf("%s listed twice", n)
		}
		seen[n] = true
		if i > 0 && names[i-1] > n {
			t.Errorf("not sorted at %s", n)
		}
	}
	for _, alias := range []string{"net", "net-alias", "dns-opt"} {
		if seen[alias] {
			t.Errorf("hidden alias %s listed; it parses as its canonical name", alias)
		}
	}
	for _, n := range []string{"network", "network-alias", "dns-option", "env", "mount", "label", "gpus", "use-api-socket"} {
		if !seen[n] {
			t.Errorf("%s missing", n)
		}
	}
}

func TestParseInterspersed(t *testing.T) {
	flags, operands, err := inspectFlags.parseInterspersed([]string{"a", "--type", "container", "b", "-s", "--", "--c"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(operands, []string{"a", "b", "--c"}) {
		t.Errorf("operands = %q", operands)
	}
	if !reflect.DeepEqual(flags, []Flag{{"type", "container"}, {"size", "true"}}) {
		t.Errorf("flags = %v", flags)
	}
}
