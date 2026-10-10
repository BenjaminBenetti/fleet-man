package backendtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// stubExecName is the backend name a Stub's exec entries carry in fleet.log.
const stubExecName = "stub"

// env is the environment of a command in c, run in dir.
func (s *Stub) env(c *stubContainer, dir string) []string {
	return []string{
		"PATH=" + filepath.Join(c.root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + c.home(),
		"USER=" + s.user,
		"LOGNAME=" + s.user,
		"SHELL=/bin/sh",
		"TMPDIR=" + filepath.Join(c.root, "tmp"),
		"TMUX_TMPDIR=" + c.tmux,
		"TERM=xterm-256color",
		"LANG=C",
		"PWD=" + dir,
		// macOS tar would add AppleDouble ._ entries for extended attributes.
		"COPYFILE_DISABLE=1",
	}
}

// command runs argv in c, in dir. It goes through sh so the command is looked
// up on the container's PATH, not the test's.
func (s *Stub) command(c *stubContainer, dir string, argv []string) *exec.Cmd {
	if len(argv) == 0 {
		return failing("stub: no command")
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", `exec "$@"`, "sh"}, argv...)...)
	cmd.Dir = dir
	cmd.Env = s.env(c, dir)
	return cmd
}

// failing is a command that prints msg to stderr and exits 1, as an exec into
// a container that is not there does.
func failing(msg string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", msg)
}

// execCmd is the command ExecCommand runs: argv in the workspace folder of
// workspaceDir's running container.
func (s *Stub) execCmd(workspaceDir string, argv []string) *exec.Cmd {
	c, running := s.forWorkspace(workspaceDir)
	switch {
	case c == nil:
		return failing("stub: no container for workspace " + workspaceDir)
	case !running:
		return failing("stub: the container for workspace " + workspaceDir + " is stopped")
	}
	return s.command(c, c.workspace, argv)
}

// Exec runs argv in the workspace's container on this process's stdio.
func (s *Stub) Exec(workspaceDir string, command []string) error {
	if err := s.begin("Exec", "", workspaceDir); err != nil {
		return err
	}
	cmd := s.execCmd(workspaceDir, command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	start := time.Now()
	err := cmd.Run()
	flog.ContainerExec(stubExecName, workspaceDir, command, time.Since(start), err)
	return err
}

// ExecCommand returns argv in the workspace's container, logging one
// "container exec" entry when run through Run/Output/CombinedOutput.
func (s *Stub) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	_ = s.begin("ExecCommand", "", workspaceDir)
	return backend.NewCmd(s.execCmd(workspaceDir, command), func(d time.Duration, err error) {
		flog.ContainerExec(stubExecName, workspaceDir, command, d, err)
	})
}

// ExecCommandQuiet is ExecCommand without the log entry.
func (s *Stub) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	_ = s.begin("ExecCommandQuiet", "", workspaceDir)
	return backend.NewCmd(s.execCmd(workspaceDir, command), nil)
}

// CopyFile streams src into the workspace's container with
// backend.StreamWriteScript, as devcontainer does. remotePath must lie in the
// container's own folders or its workspace: the write happens on this host.
func (s *Stub) CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error {
	if err := s.begin("CopyFile", "", workspaceDir); err != nil {
		return err
	}
	c, running := s.forWorkspace(workspaceDir)
	if !running {
		return fmt.Errorf("stub copy into %q: no running container for workspace %s", remotePath, workspaceDir)
	}
	if !within(remotePath, c.root) && !within(remotePath, c.workspace) {
		return fmt.Errorf("stub copy into %q: outside the stub container (%s) and its workspace", remotePath, c.root)
	}
	cmd := backend.NewCmd(s.command(c, c.workspace, backend.StreamWriteScript(remotePath, mode)), nil)
	cmd.Stdin = src
	if out, err := cmd.CombinedOutputWithTimeout(backend.CopyTimeout); err != nil {
		return fmt.Errorf("copy into %q: %w (%s)", remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Logs prints the container's log: its lifecycle commands' output.
func (s *Stub) Logs(containerID string, follow bool) error {
	if err := s.begin("Logs", containerID, ""); err != nil {
		return err
	}
	cmd := s.logsCmd(containerID, follow)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// LogsCommand returns a command printing the container's log, following it
// when follow.
func (s *Stub) LogsCommand(containerID string, follow bool) *exec.Cmd {
	_ = s.begin("LogsCommand", containerID, "")
	return s.logsCmd(containerID, follow)
}

func (s *Stub) logsCmd(containerID string, follow bool) *exec.Cmd {
	c, ok := s.lookup(containerID)
	if !ok {
		return failing("stub: no such container " + containerID)
	}
	if follow {
		return exec.Command("tail", "-n", "+1", "-f", c.logPath())
	}
	return exec.Command("cat", c.logPath())
}

// RunScript runs script with sh in the running container, in its $HOME.
func (s *Stub) RunScript(containerID, script string) (string, error) {
	if err := s.begin("RunScript", containerID, ""); err != nil {
		return "", err
	}
	c, ok := s.running(containerID)
	if !ok {
		return "", fmt.Errorf("stub: no running container %s", containerID)
	}
	out, err := s.command(c, c.home(), []string{"sh", "-c", script}).CombinedOutput()
	return string(out), err
}

// ListSessions runs backend.ListSessionsScript in the running container.
func (s *Stub) ListSessions(containerID string) string {
	_ = s.begin("ListSessions", containerID, "")
	c, ok := s.running(containerID)
	if !ok {
		return ""
	}
	out, err := s.command(c, c.home(), []string{"sh", "-c", backend.ListSessionsScript}).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// CaptureAllSessions runs backend.CaptureAllScript in the running container.
func (s *Stub) CaptureAllSessions(containerID string) backend.AllSessions {
	_ = s.begin("CaptureAllSessions", containerID, "")
	c, ok := s.running(containerID)
	if !ok {
		return backend.AllSessions{}
	}
	out, err := s.command(c, c.home(), []string{"sh", "-c", backend.CaptureAllScript}).Output()
	if err != nil {
		return backend.AllSessions{}
	}
	sessions, files, hookMissing := backend.ParseCaptureOutput(string(out))
	return backend.AllSessions{Sessions: sessions, ExtraFiles: files, HookScriptMissing: hookMissing, OK: true}
}

// stubLifecycle is the lifecycle commands a Stub runs, as argv.
type stubLifecycle struct {
	postCreate, postStart []string
}

// readStubLifecycle reads workspaceDir's devcontainer.json lifecycle commands.
// No devcontainer.json means none.
func readStubLifecycle(workspaceDir string) (stubLifecycle, error) {
	var lc stubLifecycle
	for _, rel := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		data, err := os.ReadFile(filepath.Join(workspaceDir, rel))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return lc, err
		}
		var raw struct {
			PostCreateCommand json.RawMessage `json:"postCreateCommand"`
			PostStartCommand  json.RawMessage `json:"postStartCommand"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return lc, fmt.Errorf("stub: %s (strict JSON only): %w", rel, err)
		}
		if lc.postCreate, err = lifecycleArgv(raw.PostCreateCommand); err != nil {
			return lc, fmt.Errorf("stub: %s postCreateCommand: %w", rel, err)
		}
		if lc.postStart, err = lifecycleArgv(raw.PostStartCommand); err != nil {
			return lc, fmt.Errorf("stub: %s postStartCommand: %w", rel, err)
		}
		return lc, nil
	}
	return lc, nil
}

// lifecycleArgv turns a lifecycle command's JSON (a string for sh -c, or an
// argv array) into argv; nil when it is absent.
func lifecycleArgv(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var line string
	if err := json.Unmarshal(raw, &line); err == nil {
		if line == "" {
			return nil, nil
		}
		return []string{"sh", "-c", line}, nil
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err == nil {
		return argv, nil
	}
	return nil, errors.New("only the string and array forms are supported")
}

// runLifecycle runs one lifecycle command in c's workspace, appending its
// output to c's log; a failure carries the output.
func (s *Stub) runLifecycle(c *stubContainer, name string, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	out, err := s.command(c, c.workspace, argv).CombinedOutput()
	if f, ferr := os.OpenFile(c.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
		_, _ = f.Write(out)
		_ = f.Close()
	}
	if err != nil {
		return fmt.Errorf("stub: %s failed: %w\n%s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
