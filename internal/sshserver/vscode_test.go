package sshserver

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
)

// fakeServer installs a stand-in for VS Code's server launcher under home:
// it records each `--install-extension <id>` in installs.log, fails for ids
// starting with "bad.", and never finishes for ids starting with "slow.".
func fakeServer(t *testing.T, home, rel string) (launcher, installs string) {
	t.Helper()
	launcher = filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	installs = filepath.Join(home, "installs.log")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = --install-extension ] || exit 2\n" +
		"echo \"$2\" >> " + installs + "\n" +
		"case \"$2\" in bad.*) echo \"Extension '$2' not found.\"; exit 1;; slow.*) exec sleep 30;; esac\n" +
		"echo \"Extension '$2' was successfully installed.\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return launcher, installs
}

func writeEditorConfig(t *testing.T, path string, cfg devmeta.EditorConfig) {
	t.Helper()
	cfg.V = devmeta.EditorConfigVersion
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func vscodeRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.opts.VSCode = &VSCodeOptions{Poll: 20 * time.Millisecond, ConfigWait: 2 * time.Second, InstallTimeout: 10 * time.Second,
		Owned:   func(string, string, []string) bool { return false },
		Running: func(string) bool { return true }}
	return r
}

func lines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// Once VS Code's server appears, each extension is installed once per
// generation and the settings land in the machine settings, every
// connection sharing the record of what was done.
func TestCustomizationsApplyOnceTheServerAppears(t *testing.T) {
	r := vscodeRig(t)
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "g1",
		Extensions: []string{"a.one", "bad.two"},
		Settings: map[string]json.RawMessage{
			"fleetItest.fromFixture":      json.RawMessage(`"customized"`),
			"remote.portsAttributes":      json.RawMessage(`{"17390":{"label":"Itest Forwarded","onAutoForward":"silent"}}`),
			"remote.otherPortsAttributes": json.RawMessage(`{"onAutoForward":"ignore"}`),
		}})
	_, done := r.connect()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(r.home, ".vscode-server")); err == nil {
		t.Fatal("customizations applied with no VS Code server")
	}

	_, installs := fakeServer(t, r.home, ".vscode-server/bin/abc123/bin/code-server")
	settingsPath := filepath.Join(r.home, ".vscode-server", "data", "Machine", "settings.json")
	waitFor(t, func() bool { return len(lines(installs)) == 2 && fileExists(settingsPath) })
	if got := lines(installs); !reflect.DeepEqual(got, []string{"a.one", "bad.two"}) {
		t.Fatalf("installs %q", got)
	}
	var settings map[string]any
	data, _ := os.ReadFile(settingsPath)
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json %s: %v", data, err)
	}
	if settings["fleetItest.fromFixture"] != "customized" ||
		settings["remote.portsAttributes"].(map[string]any)["17390"].(map[string]any)["label"] != "Itest Forwarded" ||
		settings["remote.otherPortsAttributes"].(map[string]any)["onAutoForward"] != "ignore" {
		t.Fatalf("settings %v", settings)
	}
	waitFor(t, func() bool { return strings.Contains(r.log.String(), "machine settings") })
	log := r.log.String()
	for _, want := range []string{"==> vscode extension a.one: installed", "warning: vscode extension bad.two:", "not found", "==> vscode machine settings:"} {
		if !strings.Contains(log, want) {
			t.Errorf("report lacks %q:\n%s", want, log)
		}
	}
	done()

	// The next connection finds it done: nothing installed again, the
	// settings unchanged.
	r.log = &syncBuffer{}
	r.opts.Log = r.log
	_, done = r.connect()
	time.Sleep(200 * time.Millisecond)
	done()
	if got := lines(installs); len(got) != 2 {
		t.Fatalf("a second connection installed again: %q", got)
	}
	if strings.Contains(r.log.String(), "installed") || strings.Contains(r.log.String(), "machine settings") {
		t.Fatalf("a second connection reported work:\n%s", r.log.String())
	}

	// A new generation (a rebuild) attempts every extension again.
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "g2", Extensions: []string{"a.one"}})
	_, done = r.connect()
	waitFor(t, func() bool { return len(lines(installs)) == 3 })
	done()
}

// A server's launcher is unpacked long before the rest of it (node_modules
// last): nothing is installed from it until the server runs, which
// Remote-SSH starts only once it is whole.
func TestCustomizationsWaitForTheServerToRun(t *testing.T) {
	r := vscodeRig(t)
	var running atomic.Bool
	var asked atomic.Value
	r.opts.VSCode.Running = func(dir string) bool {
		asked.Store(dir)
		return running.Load()
	}
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "g1", Extensions: []string{"a.one"},
		Settings: map[string]json.RawMessage{"x": json.RawMessage(`1`)}})
	launcher, installs := fakeServer(t, r.home, ".vscode-server/cli/servers/Stable-abc/server/bin/code-server")
	_, done := r.connect()
	defer done()
	waitFor(t, func() bool { return asked.Load() != nil })
	time.Sleep(200 * time.Millisecond)
	if fileExists(installs) || fileExists(filepath.Join(r.home, ".vscode-server", "data", "Machine", "settings.json")) {
		t.Fatal("customizations applied before the server ran")
	}
	if got, want := asked.Load(), filepath.Dir(filepath.Dir(launcher)); got != want {
		t.Errorf("asked whether %v runs, want %s", got, want)
	}
	running.Store(true)
	waitFor(t, func() bool { return len(lines(installs)) == 1 })
}

// An install the connection's end cuts short was not an attempt: the next
// connection installs that extension again.
func TestAnInterruptedInstallIsTriedAgain(t *testing.T) {
	r := vscodeRig(t)
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "g1", Extensions: []string{"slow.one"}})
	_, installs := fakeServer(t, r.home, ".vscode-server/bin/abc/bin/code-server")
	_, done := r.connect()
	waitFor(t, func() bool { return len(lines(installs)) == 1 })
	done()
	st := loadCustomizationState(filepath.Join(r.home, ".vscode-server", customizationStateFile))
	if result, ok := st.Extensions["slow.one"]; ok {
		t.Fatalf("an install the disconnect cut short was recorded: %q", result)
	}
	if strings.Contains(r.log.String(), "warning: vscode extension") {
		t.Errorf("a cut-short install was reported as failed:\n%s", r.log.String())
	}
	_, done = r.connect()
	waitFor(t, func() bool { return len(lines(installs)) == 2 })
	done()
}

// The exec server's layout (cli/servers/<quality>-<commit>/server) counts,
// and so does insiders'.
func TestFindServers(t *testing.T) {
	home := t.TempDir()
	stable, _ := fakeServer(t, home, ".vscode-server/cli/servers/Stable-abc/server/bin/code-server")
	legacy, _ := fakeServer(t, home, ".vscode-server/bin/def/bin/code-server")
	insiders, _ := fakeServer(t, home, ".vscode-server-insiders/bin/ghi/bin/code-server-insiders")
	if err := os.WriteFile(filepath.Join(home, ".vscode-server", "bin", "def", "bin", "notes"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range []serverFlavour{{filepath.Join(home, ".vscode-server"), "code-server"}, {filepath.Join(home, ".vscode-server-insiders"), "code-server-insiders"}} {
		for _, s := range findServers(f) {
			got[s] = true
		}
	}
	if !reflect.DeepEqual(got, map[string]bool{stable: true, legacy: true, insiders: true}) {
		t.Fatalf("servers %v", got)
	}
}

// A connection that forwards to an endpoint VS Code's server owns is a VS
// Code connection: postAttachCommand runs once for it, in the workspace
// folder; a connection that does not is not.
func TestPostAttachRunsOncePerVSCodeConnection(t *testing.T) {
	r := vscodeRig(t)
	workspace := t.TempDir()
	out := filepath.Join(t.TempDir(), "attach.log")
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "g1", WorkspaceFolder: workspace,
		PostAttach: []json.RawMessage{
			json.RawMessage(`"pwd >> ` + out + `; echo first-layer"`),
			json.RawMessage(`{"a":["sh","-c","echo named >> ` + out + `"],"b":"echo b-ran"}`),
		}})
	fakeServer(t, r.home, ".vscode-server/bin/abc/bin/code-server")

	vscodePort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer vscodePort.Close()
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, l := range []net.Listener{vscodePort, other} {
		go func(l net.Listener) {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}(l)
	}
	var askedDirs []string
	r.opts.VSCode.Owned = func(network, address string, dirs []string) bool {
		askedDirs = dirs
		return network == "tcp" && address == vscodePort.Addr().String()
	}

	// A plain connection, forwarding elsewhere: no postAttachCommand.
	c, done := r.connect()
	if conn, err := c.Dial("tcp", other.Addr().String()); err == nil {
		_ = conn.Close()
	}
	_, _, _ = run(t, c, nil, "true")
	time.Sleep(200 * time.Millisecond)
	done()
	if fileExists(out) {
		t.Fatal("postAttachCommand ran for a connection that is not VS Code's")
	}

	// A VS Code connection: once, however many forwards it opens.
	c, done = r.connect()
	for range 3 {
		conn, err := c.Dial("tcp", vscodePort.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	waitFor(t, func() bool { return strings.Contains(r.log.String(), "==> postAttachCommand: done") })
	done()
	// pwd prints the folder resolved (macOS's /var is /private/var).
	if real, _ := filepath.EvalSymlinks(workspace); !reflect.DeepEqual(lines(out), []string{real, "named"}) {
		t.Fatalf("postAttachCommand wrote %q", lines(out))
	}
	if real, _ := filepath.EvalSymlinks(filepath.Join(r.home, ".vscode-server")); len(askedDirs) != 1 || askedDirs[0] != real {
		t.Errorf("owner asked about %q", askedDirs)
	}
	for _, want := range []string{"==> postAttachCommand (VS Code attached)", "first-layer", "b: b-ran"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, r.log.String())
		}
	}

	// The next VS Code connection runs it again.
	c, done = r.connect()
	conn, err := c.Dial("tcp", vscodePort.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	waitFor(t, func() bool { return len(lines(out)) == 4 })
	done()
}

// A missing editor config is reported once, and nothing is applied.
func TestNoEditorConfig(t *testing.T) {
	r := vscodeRig(t)
	r.opts.VSCode.ConfigWait = 100 * time.Millisecond
	_, installs := fakeServer(t, r.home, ".vscode-server/bin/abc/bin/code-server")
	_, done := r.connect()
	waitFor(t, func() bool { return strings.Contains(r.log.String(), "no editor settings") })
	done()
	if fileExists(installs) || fileExists(filepath.Join(r.home, ".vscode-server", "data", "Machine", "settings.json")) {
		t.Fatal("customizations applied without a config")
	}
	if n := strings.Count(r.log.String(), "no editor settings"); n != 1 {
		t.Errorf("reported %d times:\n%s", n, r.log.String())
	}
}

func TestMergeMachineSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "Machine", "settings.json")
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	want := map[string]json.RawMessage{
		"a":     raw(`1`),
		"b":     raw(`"fleet"`),
		"ports": raw(`{"1":{"label":"one"},"2":{"label":"two"}}`),
	}
	changed, err := mergeMachineSettings(path, nil, want)
	if err != nil || !changed {
		t.Fatalf("first merge: %v %v", changed, err)
	}
	if got := readJSON(t, path); !reflect.DeepEqual(got, map[string]any{"a": 1.0, "b": "fleet", "ports": map[string]any{"1": map[string]any{"label": "one"}, "2": map[string]any{"label": "two"}}}) {
		t.Fatalf("written %v", got)
	}
	if changed, _ := mergeMachineSettings(path, want, want); changed {
		t.Fatal("an unchanged merge rewrote the file")
	}

	// The user edits: changes b, removes a, adds their own key and a port,
	// in an order of their own.
	user := "{\n\t\"mine\": true,\n\t\"b\": \"user\",\n\t\"ports\": {\"2\": {\"label\": \"two\"}, \"1\": {\"label\": \"one\"}, \"9\": {\"label\": \"nine\"}}\n}\n"
	if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// The devcontainer now wants: a=2 (fleet's a was removed by the user, but
	// the value is new), b="fleet2" (the user's b stays), port 1 relabelled,
	// c new, and no longer anything else of fleet's.
	next := map[string]json.RawMessage{
		"a":     raw(`2`),
		"b":     raw(`"fleet2"`),
		"c":     raw(`[1,2]`),
		"ports": raw(`{"1":{"label":"ONE"},"2":{"label":"two"}}`),
	}
	changed, err = mergeMachineSettings(path, want, next)
	if err != nil || !changed {
		t.Fatalf("second merge: %v %v", changed, err)
	}
	got := readJSON(t, path)
	wantGot := map[string]any{
		"mine": true, "b": "user", "a": 2.0, "c": []any{1.0, 2.0},
		"ports": map[string]any{"1": map[string]any{"label": "ONE"}, "2": map[string]any{"label": "two"}, "9": map[string]any{"label": "nine"}},
	}
	if !reflect.DeepEqual(got, wantGot) {
		t.Fatalf("merged %v\nwant %v", got, wantGot)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "{\n\t\"mine\": true,\n\t\"b\": \"user\",") {
		t.Errorf("the user's key order was lost:\n%s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}

	// A key the user removed is not put back while fleet's value is unchanged.
	edited := readJSON(t, path)
	delete(edited, "c")
	stripped, _ := json.Marshal(edited)
	if err := os.WriteFile(path, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := mergeMachineSettings(path, next, next); err != nil || changed {
		t.Fatalf("re-adding a removed key: %v %v", changed, err)
	}

	// Settings fleet wrote that are no longer wanted go, unless changed.
	if _, err := mergeMachineSettings(path, next, map[string]json.RawMessage{"b": raw(`"fleet2"`)}); err != nil {
		t.Fatal(err)
	}
	got = readJSON(t, path)
	if _, ok := got["a"]; ok {
		t.Errorf("fleet's a was kept: %v", got)
	}
	if got["b"] != "user" || got["mine"] != true {
		t.Errorf("user settings lost: %v", got)
	}

	// A file with comments is left alone.
	jsonc := "{\n\t// mine\n\t\"x\": 1,\n}\n"
	if err := os.WriteFile(path, []byte(jsonc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeMachineSettings(path, nil, want); err == nil || !strings.Contains(err.Error(), "not plain JSON") {
		t.Fatalf("JSONC: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != jsonc {
		t.Fatal("a commented settings file was rewritten")
	}
}

func TestLifecycleCommands(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []lifecycleCommand
		bad  bool
	}{
		{`"echo hi"`, []lifecycleCommand{{argv: []string{"/bin/sh", "-c", "echo hi"}}}, false},
		{`["echo","a b"]`, []lifecycleCommand{{argv: []string{"echo", "a b"}}}, false},
		{`{"z":"echo z","a":["echo","a"]}`, []lifecycleCommand{{name: "a", argv: []string{"echo", "a"}}, {name: "z", argv: []string{"/bin/sh", "-c", "echo z"}}}, false},
		{`""`, nil, false},
		{`[]`, nil, false},
		{`42`, nil, true},
		{`{"a":7}`, nil, true},
	} {
		got, err := lifecycleCommands(json.RawMessage(tc.raw))
		if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("lifecycleCommands(%s) = %+v, %v", tc.raw, got, err)
		}
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return v
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
