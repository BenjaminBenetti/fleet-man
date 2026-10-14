//go:build linux || darwin || freebsd

package podspec

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Bind classification against the real filesystem (Input.Host nil): each
// kind of daemon-host source gets its kind, and a FIFO is refused.
func TestBindClassificationOnTheRealFilesystem(t *testing.T) {
	// A short directory: unix socket paths are limited to ~100 bytes.
	dir, err := os.MkdirTemp("", "podspec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	mustDo := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	listen := func(path string) {
		l, err := net.Listen("unix", path)
		mustDo(err)
		t.Cleanup(func() { l.Close() })
	}
	mustDo(os.Mkdir(filepath.Join(dir, "conf"), 0o755))
	mustDo(os.WriteFile(filepath.Join(dir, "conf", "a"), nil, 0o644))
	mustDo(os.WriteFile(filepath.Join(dir, "gitconfig"), nil, 0o644))
	mustDo(os.Mkdir(filepath.Join(dir, "x11"), 0o755))
	listen(filepath.Join(dir, "x11", "X0"))
	listen(filepath.Join(dir, "gpg.sock"))
	listen(filepath.Join(dir, "agent.sock"))
	listen(filepath.Join(dir, "docker.sock"))
	mustDo(os.Symlink(filepath.Join(dir, "gpg.sock"), filepath.Join(dir, "gpg-link")))
	mustDo(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600))

	in := baseInput([]string{
		"--mount", "type=bind,source=" + filepath.Join(dir, "conf") + ",target=/conf",
		"--mount", "type=bind,source=" + filepath.Join(dir, "gitconfig") + ",target=/gitconfig",
		"--mount", "type=bind,source=" + filepath.Join(dir, "x11") + ",target=/tmp/.X11-unix",
		"--mount", "type=bind,source=" + filepath.Join(dir, "gpg-link") + ",target=/gpg",
		"--mount", "type=bind,source=" + filepath.Join(dir, "agent.sock") + ",target=/agent",
		"--mount", "type=bind,source=" + filepath.Join(dir, "docker.sock") + ",target=/var/run/docker.sock",
		"-v", filepath.Join(dir, "created-by-docker") + ":/new",
	}, nil, nil)
	in.Host = nil
	in.Facts.Local = true
	in.HostAgentSockets = []string{filepath.Join(dir, "agent.sock") + "/"}
	res, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]MountKind{
		"/conf": MountHostDir, "/gitconfig": MountHostFile, "/tmp/.X11-unix": MountHostSocketDir,
		"/gpg": MountHostSocket, "/agent": MountSSHAgent, "/var/run/docker.sock": MountDockerSocket,
		"/new": MountHostDir, "/workspaces/app": MountWorkspace,
	}
	for _, m := range res.Pod.Mounts {
		if want[m.Target] != m.Kind {
			t.Errorf("%s is %s, want %s", m.Target, m.Kind, want[m.Target])
		}
		delete(want, m.Target)
	}
	if len(want) > 0 {
		t.Errorf("no mount at %v", want)
	}
	if res.Pod.DooD == nil || !res.Pod.DooD.Privileged {
		t.Error("a docker.sock bind does not get the docker engine's sidecar")
	}

	in = baseInput([]string{"--mount", "type=bind,source=" + filepath.Join(dir, "fifo") + ",target=/fifo"}, nil, nil)
	in.Host = nil
	if _, err := Translate(in); len(Refusals(err)) != 1 || Refusals(err)[0].Code != CodeHostSpecialFile {
		t.Errorf("a FIFO bind: %v", err)
	}
}
