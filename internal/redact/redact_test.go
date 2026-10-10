package redact

import (
	"reflect"
	"strings"
	"testing"
)

func TestEnvArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "separate value",
			in:   []string{"run", "-e", "API_KEY=sk-123", "img"},
			want: []string{"run", "-e", "API_KEY=<redacted>", "img"},
		},
		{
			name: "long flag",
			in:   []string{"run", "--env", "TOKEN=abc", "img"},
			want: []string{"run", "--env", "TOKEN=<redacted>", "img"},
		},
		{
			name: "long flag with equals",
			in:   []string{"run", "--env=TOKEN=a=b", "img"},
			want: []string{"run", "--env=TOKEN=<redacted>", "img"},
		},
		{
			name: "attached short flag",
			in:   []string{"run", "-eTOKEN=abc", "img"},
			want: []string{"run", "-eTOKEN=<redacted>", "img"},
		},
		{
			name: "value with spaces and an equals sign",
			in:   []string{"-e", "GREETING=hello there=you"},
			want: []string{"-e", "GREETING=<redacted>"},
		},
		{
			name: "empty value",
			in:   []string{"-e", "EMPTY="},
			want: []string{"-e", "EMPTY=<redacted>"},
		},
		{
			name: "pass-through from the environment",
			in:   []string{"run", "-e", "HOME", "img"},
			want: []string{"run", "-e", "HOME", "img"},
		},
		{
			name: "flag at the end",
			in:   []string{"run", "-e"},
			want: []string{"run", "-e"},
		},
		{
			name: "env file path is not a value",
			in:   []string{"run", "--env-file", "/tmp/x.env", "--env-file=/a=b"},
			want: []string{"run", "--env-file", "/tmp/x.env", "--env-file=/a=b"},
		},
		{
			name: "other flags with equals",
			in:   []string{"--label", "a=b", "--mount", "type=bind,source=/a,target=/b", "-l", "k=v"},
			want: []string{"--label", "a=b", "--mount", "type=bind,source=/a,target=/b", "-l", "k=v"},
		},
		{
			name: "consecutive env flags",
			in:   []string{"-e", "A=1", "-e", "B=2", "--env=C=3"},
			want: []string{"-e", "A=<redacted>", "-e", "B=<redacted>", "--env=C=<redacted>"},
		},
		{
			name: "word starting with -e that is not a variable",
			in:   []string{"-exec=1", "-e1=x"},
			want: []string{"-exec=<redacted>", "-e1=x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.in...)
			if got := EnvArgs(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EnvArgs(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !reflect.DeepEqual(tt.in, in) {
				t.Errorf("EnvArgs changed its argument: %q", tt.in)
			}
		})
	}
}

func TestEnvLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "devcontainer cli docker run",
			in:   "[2026-10-10T00:00:00.000Z] Start: Run: docker run --sig-proxy=false -a STDOUT -a STDERR --mount type=bind,source=/ws,target=/workspaces/ws -l devcontainer.local_folder=/ws -e API_KEY=sk-live-123 -e DEBUG=1 --entrypoint /bin/sh vsc-ws-abc -c echo Container started",
			want: "[2026-10-10T00:00:00.000Z] Start: Run: docker run --sig-proxy=false -a STDOUT -a STDERR --mount type=bind,source=/ws,target=/workspaces/ws -l devcontainer.local_folder=/ws -e API_KEY=<redacted> -e DEBUG=<redacted> --entrypoint /bin/sh vsc-ws-abc -c echo Container started",
		},
		{
			name: "a value with spaces runs to the next flag",
			in:   "docker run -e GREETING=hello big world -u node img",
			want: "docker run -e GREETING=<redacted> -u node img",
		},
		{
			name: "a value at the end of the line",
			in:   "run -e SECRET=a b c",
			want: "run -e SECRET=<redacted>",
		},
		{
			name: "long and attached forms",
			in:   "run --env TOKEN=x --env=OTHER=y -eLAST=z --rm",
			want: "run --env TOKEN=<redacted> --env=OTHER=<redacted> -eLAST=<redacted> --rm",
		},
		{
			name: "pass-through is kept",
			in:   "run -e HOME -e PATH img",
			want: "run -e HOME -e PATH img",
		},
		{
			name: "spacing outside values is kept",
			in:   "  a\t-e K=v   --rm  ",
			want: "  a\t-e K=<redacted>   --rm  ",
		},
		{
			name: "text without env flags is untouched",
			in:   "Step 3/7 : RUN set -e && make",
			want: "Step 3/7 : RUN set -e && make",
		},
		{
			name: "no flags at all",
			in:   "Container started",
			want: "Container started",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnvLine(tt.in); got != tt.want {
				t.Errorf("EnvLine(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEnvText(t *testing.T) {
	in := "devcontainer up failed\nRun: docker run -e A=secret-a img\nerror: boom -e B=secret-b\n"
	got := EnvText(in)
	for _, secret := range []string{"secret-a", "secret-b"} {
		if strings.Contains(got, secret) {
			t.Errorf("EnvText kept %q:\n%s", secret, got)
		}
	}
	want := "devcontainer up failed\nRun: docker run -e A=<redacted>\nerror: boom -e B=<redacted>\n"
	if got != want {
		t.Errorf("EnvText = %q, want %q", got, want)
	}
}

// FuzzEnvLineHidesValues checks the property the creation log relies on: after
// "-e KEY=" nothing of the value survives.
func FuzzEnvLineHidesValues(f *testing.F) {
	f.Add("docker run", "API_KEY", "sk-123", "--rm img")
	f.Add("", "K", "a b c", "")
	f.Add("x -e", "B", "two words", "-u root")
	f.Fuzz(func(t *testing.T, prefix, key, value, suffix string) {
		if !isEnvName(key) || strings.ContainsAny(value, "\n\t-") || len(value) < 3 {
			t.Skip()
		}
		line := prefix + " -e " + key + "=" + value + " " + suffix
		got := EnvLine(line)
		if strings.Contains(got, key+"="+value) {
			t.Fatalf("EnvLine(%q) = %q still holds the value", line, got)
		}
	})
}
