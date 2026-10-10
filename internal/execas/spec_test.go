package execas

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSpecFile(t *testing.T, data string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "exec.json")
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLoadSpec(t *testing.T) {
	file := writeSpecFile(t, `{
		"remote_user": "vscode",
		"container_user": "node:node",
		"workspace_folder": "/workspaces/proj",
		"working_dir": "/srv",
		"user_env_probe": "loginShell",
		"remote_env": {"PATH": "${containerEnv:PATH}:/extra"},
		"ssh_auth_sock": "/fleet-mounts/control/ssh-agent.sock",
		"a_field_from_a_newer_daemon": {"x": 1}
	}`)
	got, err := LoadSpec(file)
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{
		RemoteUser:      "vscode",
		ContainerUser:   "node:node",
		WorkspaceFolder: "/workspaces/proj",
		WorkingDir:      "/srv",
		UserEnvProbe:    ProbeLoginShell,
		RemoteEnv:       map[string]string{"PATH": "${containerEnv:PATH}:/extra"},
		SSHAuthSock:     "/fleet-mounts/control/ssh-agent.sock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadSpec:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadSpecRefusesBadSpecs(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"not json", "{", "parse exec spec"},
		{"unknown probe", `{"user_env_probe":"bashrc"}`, "unknown user_env_probe"},
		{"relative workspace", `{"workspace_folder":"workspaces/x"}`, "not an absolute path"},
		{"relative working dir", `{"working_dir":"."}`, "not an absolute path"},
		{"relative agent socket", `{"ssh_auth_sock":"agent.sock"}`, "not an absolute path"},
		{"bad remoteEnv name", `{"remote_env":{"A=B":"x"}}`, "contains '='"},
		{"empty remoteEnv name", `{"remote_env":{"":"x"}}`, "empty variable name"},
		{"NUL in a value", `{"remote_env":{"A":"x\u0000y"}}`, "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSpec(writeSpecFile(t, tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := LoadSpec(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing spec loaded")
	}
	big := writeSpecFile(t, `{"remote_user":"`+strings.Repeat("a", maxSpecSize)+`"}`)
	if _, err := LoadSpec(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversize spec: err = %v", err)
	}
}

func TestProbeModes(t *testing.T) {
	if ProbeMode("").Effective() != ProbeLoginInteractiveShell {
		t.Fatal("the CLI's default probe is loginInteractiveShell")
	}
	for _, m := range []ProbeMode{"", ProbeNone, ProbeLoginShell, ProbeInteractiveShell, ProbeLoginInteractiveShell} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
		if m != "" && m.Effective() != m {
			t.Errorf("%q.Effective() = %q", m, m.Effective())
		}
	}
	if ProbeMode("login").Valid() {
		t.Fatal("an unknown mode is valid")
	}
}
