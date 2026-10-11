package sshserver

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
)

// The customizations against a real VS Code server, when one is at hand:
//
//	FLEET_VSCODE_SERVER_TARBALL=<vscode-server-linux-x64.tar.gz> \
//	  [FLEET_VSCODE_EXTENSION=<marketplace id>] go test -run TestRealVSCodeServer ./internal/sshserver
//
// (https://update.code.visualstudio.com/latest/server-linux-x64/stable).
// The server is unpacked where Remote-SSH puts it, ~/.vscode-server/bin/<commit>;
// its own `code-server --install-extension` installs the extension from the
// marketplace (network) and fails a made-up one; the machine settings land
// where the server reads them; and a forward to the real server's port is
// found to be VS Code's through /proc, which runs postAttachCommand.
func TestRealVSCodeServer(t *testing.T) {
	tarball := os.Getenv("FLEET_VSCODE_SERVER_TARBALL")
	if tarball == "" {
		t.Skip("FLEET_VSCODE_SERVER_TARBALL names no VS Code server download")
	}
	ext := os.Getenv("FLEET_VSCODE_EXTENSION")
	if ext == "" {
		ext = "EditorConfig.EditorConfig"
	}
	// Both of Remote-SSH's layouts: the server alone, and under the exec
	// server it installs by default now.
	for name, dir := range map[string]func(commit string) string{
		"bin": func(commit string) string { return filepath.Join("bin", commit) },
		"cli": func(commit string) string { return filepath.Join("cli", "servers", "Stable-"+commit, "server") },
	} {
		t.Run(name, func(t *testing.T) { realVSCodeServer(t, tarball, ext, dir) })
	}
}

func realVSCodeServer(t *testing.T, tarball, ext string, layout func(commit string) string) {
	r := newRig(t)
	r.opts.VSCode = &VSCodeOptions{Poll: 200 * time.Millisecond, ConfigWait: 5 * time.Second, InstallTimeout: 5 * time.Minute}

	// Unpack it as Remote-SSH does.
	product := filepath.Join(t.TempDir(), "product.json")
	out, err := exec.Command("sh", "-c", `tar -xzf "$1" -O vscode-server-linux-x64/product.json > "$2"`, "sh", tarball, product).CombinedOutput()
	if err != nil {
		t.Fatalf("product.json: %v %s", err, out)
	}
	var p struct{ Commit string }
	data, _ := os.ReadFile(product)
	if err := json.Unmarshal(data, &p); err != nil || p.Commit == "" {
		t.Fatalf("product.json %s: %v", data, err)
	}
	root := filepath.Join(r.home, ".vscode-server", layout(p.Commit))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-xzf", tarball, "-C", root, "--strip-components", "1").CombinedOutput(); err != nil {
		t.Fatalf("unpack: %v %s", err, out)
	}
	server := filepath.Join(root, "bin", "code-server")

	marker := filepath.Join(t.TempDir(), "attached")
	writeEditorConfig(t, r.opts.EditorConfig, devmeta.EditorConfig{Gen: "real", WorkspaceFolder: r.home,
		Extensions: []string{ext, "fleet-itest.no-such-extension"},
		Settings: map[string]json.RawMessage{
			"editor.tabSize":         json.RawMessage(`3`),
			"remote.portsAttributes": json.RawMessage(`{"17390":{"label":"Itest Forwarded","onAutoForward":"silent"}}`),
		},
		PostAttach: []json.RawMessage{json.RawMessage(`"echo attached > ` + marker + `"`)},
	})

	c, done := r.connect()
	statePath := filepath.Join(r.home, ".vscode-server", customizationStateFile)
	deadline := time.Now().Add(6 * time.Minute)
	for {
		st := loadCustomizationState(statePath)
		if len(st.Extensions) == 2 && st.Settings != nil {
			if st.Extensions[strings.ToLower(ext)] != "installed" {
				t.Fatalf("%s: %q\n%s", ext, st.Extensions[strings.ToLower(ext)], r.log.String())
			}
			if !strings.HasPrefix(st.Extensions["fleet-itest.no-such-extension"], "failed") {
				t.Fatalf("a made-up extension: %q", st.Extensions["fleet-itest.no-such-extension"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the installs never finished: %+v\n%s", st, r.log.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("report:\n%s", r.log.String())
	list := exec.Command(server, "--list-extensions")
	list.Env = append(os.Environ(), "HOME="+r.home)
	listed, err := list.CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(listed)), strings.ToLower(ext)) {
		t.Fatalf("the server does not list %s: %v\n%s", ext, err, listed)
	}
	settings := readJSON(t, filepath.Join(r.home, ".vscode-server", "data", "Machine", "settings.json"))
	if settings["editor.tabSize"] != 3.0 || settings["remote.portsAttributes"] == nil {
		t.Fatalf("machine settings %v", settings)
	}

	// Run the real server and forward to it: a VS Code connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := exec.CommandContext(ctx, server, "--host", "127.0.0.1", "--port", "0", "--without-connection-token", "--accept-server-license-terms")
	srv.Env = append(os.Environ(), "HOME="+r.home)
	srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	srv.Cancel = func() error { return syscall.Kill(-srv.Process.Pid, syscall.SIGKILL) }
	stdout, _ := srv.StdoutPipe()
	srv.Stderr = srv.Stdout
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Wait()
		// The server's children (its extension host) go with its group.
		_ = syscall.Kill(-srv.Process.Pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	})
	portRE := regexp.MustCompile(`listening on (\d+)`)
	port := ""
	sc := bufio.NewScanner(stdout)
	for port == "" && sc.Scan() {
		if m := portRE.FindStringSubmatch(sc.Text()); m != nil {
			port = m[1]
		}
	}
	if port == "" {
		t.Fatal("the server never said its port")
	}
	go func() {
		for sc.Scan() {
		}
	}()
	conn, err := c.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitFor(t, func() bool { return fileExists(marker) })
	done()
}
