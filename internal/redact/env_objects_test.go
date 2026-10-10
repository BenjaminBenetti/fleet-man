package redact

import (
	"strings"
	"testing"
)

func TestEnvObjects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "metadata label on the docker run line",
			in:   `-l devcontainer.metadata=[{"remoteUser":"vscode"},{"postCreateCommand":"make","containerEnv":{"API_KEY":"sk-123","DEBUG":"1"}}] vsc-img -c echo`,
			want: `-l devcontainer.metadata=[{"remoteUser":"vscode"},{"postCreateCommand":"make","containerEnv":{"API_KEY":"<redacted>","DEBUG":"<redacted>"}}] vsc-img -c echo`,
		},
		{
			name: "remoteEnv with null and spaces",
			in:   `{"remoteEnv": { "TOKEN" : "a b", "UNSET": null , "PATH":"${containerEnv:PATH}:/x"}, "x": 1}`,
			want: `{"remoteEnv": { "TOKEN" : "<redacted>", "UNSET": null , "PATH":"<redacted>"}, "x": 1}`,
		},
		{
			name: "escaped quotes, braces and backslashes inside values",
			in:   `{"containerEnv":{"A":"x\"y}z","B":"c:\\d"}}`,
			want: `{"containerEnv":{"A":"<redacted>","B":"<redacted>"}}`,
		},
		{
			name: "several entries each with their own containerEnv",
			in:   `[{"containerEnv":{"A":"1"}},{"containerEnv":{"B":"2"}}]`,
			want: `[{"containerEnv":{"A":"<redacted>"}},{"containerEnv":{"B":"<redacted>"}}]`,
		},
		{
			name: "escaped form from a generated Dockerfile label",
			in:   `LABEL devcontainer.metadata="[{\"containerEnv\":{\"API_KEY\":\"sk-\$HOME\\\"q\",\"B\":\"2\"},\"remoteUser\":\"node\"}]"`,
			want: `LABEL devcontainer.metadata="[{\"containerEnv\":{\"API_KEY\":\"<redacted>\",\"B\":\"<redacted>\"},\"remoteUser\":\"node\"}]"`,
		},
		{
			name: "a value cut off by the end of the line",
			in:   `{"containerEnv":{"A":"visible","B":"cut off here`,
			want: `{"containerEnv":{"A":"<redacted>","B":"<redacted>`,
		},
		{
			name: "the word outside a JSON key is left alone",
			in:   `setting containerEnv for remoteEnv: "x"`,
			want: `setting containerEnv for remoteEnv: "x"`,
		},
		{
			name: "a key whose value is not an object",
			in:   `{"containerEnv":"oops","remoteEnv":{"K":"v"}}`,
			want: `{"containerEnv":"oops","remoteEnv":{"K":"<redacted>"}}`,
		},
		{
			name: "empty object",
			in:   `{"containerEnv":{}}`,
			want: `{"containerEnv":{}}`,
		},
		{
			name: "no metadata",
			in:   `Container started`,
			want: `Container started`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnvObjects(tt.in); got != tt.want {
				t.Errorf("EnvObjects(%s)\n got %s\nwant %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestEnvLineHidesTheMetadataCopy: the docker run line carries each value
// twice, as an -e flag and in the metadata label, and EnvLine hides both.
func TestEnvLineHidesTheMetadataCopy(t *testing.T) {
	line := `[2026-10-10T06:04:12.787Z] Start: Run: docker run --sig-proxy=false -l devcontainer.local_folder=/ws -e FLEET_ITEST_SECRET=hunter2 --entrypoint /bin/sh -l devcontainer.metadata=[{"postCreateCommand":"sudo apt-get update -qq","containerEnv":{"FLEET_ITEST_SECRET":"hunter2"}}] vsc-x-uid -c echo Container started`
	got := EnvLine(line)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("EnvLine kept the value:\n%s", got)
	}
	for _, want := range []string{"-e FLEET_ITEST_SECRET=<redacted> --entrypoint /bin/sh", `"containerEnv":{"FLEET_ITEST_SECRET":"<redacted>"}`, "vsc-x-uid -c echo Container started"} {
		if !strings.Contains(got, want) {
			t.Errorf("EnvLine result is missing %q:\n%s", want, got)
		}
	}
	args := EnvArgs([]string{"-e", "K=hunter2", "-l", `devcontainer.metadata=[{"containerEnv":{"K":"hunter2"}}]`})
	if strings.Contains(strings.Join(args, " "), "hunter2") {
		t.Fatalf("EnvArgs kept the value: %q", args)
	}
}

// FuzzEnvObjectsHidesValues: whatever the value, it does not survive.
func FuzzEnvObjectsHidesValues(f *testing.F) {
	f.Add("sk-123", false)
	f.Add(`a"b`, true)
	f.Add(`x}y\z`, false)
	f.Fuzz(func(t *testing.T, value string, escaped bool) {
		if len(value) < 4 || strings.ContainsAny(value, "\n") {
			t.Skip()
		}
		enc := jsonQuote(value)
		line := `[{"x":1,"containerEnv":{"K":` + enc + `}}]`
		if escaped {
			line = `LABEL devcontainer.metadata="` + escapeLabel(line) + `"`
		}
		got := EnvObjects(line)
		if strings.Contains(got, enc) || (escaped && strings.Contains(got, escapeLabel(enc))) {
			t.Fatalf("EnvObjects(%s) = %s still holds the value", line, got)
		}
		if !strings.Contains(got, Placeholder) {
			t.Fatalf("EnvObjects(%s) = %s hid nothing", line, got)
		}
	})
}

// jsonQuote is a JSON string literal of s (ASCII escapes only).
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// escapeLabel escapes s the way the devcontainer CLI writes metadata into a
// Dockerfile LABEL: a backslash before every quote, backslash and dollar.
func escapeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '"' || r == '\\' || r == '$' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
