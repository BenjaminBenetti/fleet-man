package fleet

import (
	"strings"
	"testing"
)

func TestValidatePluginEnv(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantErr string // "" = valid
	}{
		{"empty", nil, ""},
		{"pass-through names", []string{"AWS_PROFILE", "AWS_SECRET_ACCESS_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"plain values", []string{"AWS_PROFILE=dev", "AWS_REGION=eu-west-1", "EMPTY="}, ""},
		{"value with =", []string{"OPTS=a=b"}, ""},
		{"lowercase name", []string{"http_proxy"}, ""},
		{"secret value", []string{"AWS_SECRET_ACCESS_KEY=abc"}, "looks like a secret"},
		{"token value", []string{"GITHUB_TOKEN=ghp"}, "looks like a secret"},
		{"password value", []string{"Db_Password=x"}, "looks like a secret"},
		{"key value", []string{"API_KEY=x"}, "looks like a secret"},
		{"empty secret value still a value", []string{"MY_TOKEN="}, "looks like a secret"},
		{"bad name", []string{"1ABC"}, "not a variable name"},
		{"name with dash", []string{"A-B=1"}, "not a variable name"},
		{"empty name", []string{"=x"}, "not a variable name"},
		{"blank entry", []string{""}, "not a variable name"},
		{"PATH", []string{"PATH=/x"}, "extra_path"},
		{"PATH pass-through", []string{"PATH"}, "extra_path"},
		{"exec info", []string{"KUBERNETES_EXEC_INFO"}, "cannot be set"},
		{"duplicate", []string{"AWS_PROFILE", "AWS_PROFILE=dev"}, "listed twice"},
		{"NUL", []string{"A=b\x00c"}, "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePluginEnv(tt.entries)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ValidatePluginEnv(%q) = %v, want ok", tt.entries, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("ValidatePluginEnv(%q) = %v, want an error containing %q", tt.entries, err, tt.wantErr)
			}
		})
	}
}

// A refused secret value is never echoed back in the error.
func TestValidatePluginEnvDoesNotEchoSecrets(t *testing.T) {
	err := ValidatePluginEnv([]string{"MY_SECRET=hunter2"})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v must refuse without echoing the value", err)
	}
}

func TestParsePluginEnv(t *testing.T) {
	if n, v, ok := ParsePluginEnv("A=b=c"); n != "A" || v != "b=c" || !ok {
		t.Errorf("ParsePluginEnv(A=b=c) = %q %q %v", n, v, ok)
	}
	if n, v, ok := ParsePluginEnv("A"); n != "A" || v != "" || ok {
		t.Errorf("ParsePluginEnv(A) = %q %q %v", n, v, ok)
	}
}

func TestValidateExtraPath(t *testing.T) {
	for _, tt := range []struct {
		dirs []string
		ok   bool
	}{
		{nil, true},
		{[]string{"/opt/google-cloud-sdk/bin", "/home/me/.local/bin"}, true},
		{[]string{"bin"}, false},
		{[]string{"~/bin"}, false},
		{[]string{"/a:/b"}, false},
		{[]string{""}, false},
	} {
		if err := ValidateExtraPath(tt.dirs); (err == nil) != tt.ok {
			t.Errorf("ValidateExtraPath(%q) = %v, want ok=%v", tt.dirs, err, tt.ok)
		}
	}
}
