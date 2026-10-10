package devcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/redact"
)

// Tools names the programs a devcontainer CLI run uses. The zero value finds
// them the way fleetd would: the CLI and docker on PATH, and fleetd's own
// executable for the shim links.
type Tools struct {
	// CLI is the devcontainer CLI; "" means "devcontainer" on PATH.
	CLI string
	// Docker is the real docker capture mode hands builds to; "" means
	// "docker" on PATH.
	Docker string
	// Exe is the fleet executable the shim links point at; "" means
	// os.Executable().
	Exe string
}

func (t Tools) cli() (string, error) {
	if t.CLI != "" {
		return t.CLI, nil
	}
	path, err := exec.LookPath("devcontainer")
	if err != nil {
		return "", fmt.Errorf("the devcontainer CLI is not installed on the daemon host: %w", err)
	}
	return path, nil
}

func (t Tools) docker() (string, error) {
	path := t.Docker
	if path == "" {
		found, err := exec.LookPath("docker")
		if err != nil {
			return "", fmt.Errorf("docker is not installed on the daemon host: %w", err)
		}
		path = found
	}
	return filepath.Abs(path)
}

func (t Tools) exe() (string, error) {
	if t.Exe != "" {
		return filepath.Abs(t.Exe)
	}
	return os.Executable()
}

// shimDir creates a shim directory for this run.
func (t Tools) shimDir(cfg kubeshim.Config) (*kubeshim.Dir, error) {
	exe, err := t.exe()
	if err != nil {
		return nil, err
	}
	dir, err := kubeshim.NewDir(exe)
	if err != nil {
		return nil, err
	}
	if cfg.Mode == kubeshim.ModePod && cfg.Pod != nil && cfg.Pod.ExecSocket == "" {
		cfg.Pod.ExecSocket = dir.ExecSocket()
	}
	if err := dir.Configure(cfg); err != nil {
		_ = dir.Remove()
		return nil, err
	}
	return dir, nil
}

// cliRun is one finished devcontainer CLI run.
type cliRun struct {
	stdout []byte
	// log is the CLI's log (its stderr).
	log string
	err error
}

// cliWaitDelay bounds how long a run waits for the CLI's output pipes after
// it exited, should a grandchild still hold them.
const cliWaitDelay = 10 * time.Second

// run runs the devcontainer CLI with args and extraEnv over fleetd's own
// environment, in a private TMPDIR, teeing its log to jobLog. The CLI and
// everything it starts form one process group, which a cancelled ctx kills.
func (t Tools) run(ctx context.Context, args, extraEnv []string, jobLog io.Writer) cliRun {
	cli, err := t.cli()
	if err != nil {
		return cliRun{err: err}
	}
	// The CLI materialises fixed-name scratch files in $TMPDIR, so
	// concurrent runs need their own (the devcontainer backend's #63 fix).
	tmp, err := os.MkdirTemp("", "fleet-devcontainer-")
	if err != nil {
		return cliRun{err: err}
	}
	defer os.RemoveAll(tmp)

	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = append(withoutEnv(os.Environ(), "TMPDIR"), "TMPDIR="+tmp)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The whole group: the docker builds the CLI started would outlive
		// it otherwise. (A cancel racing the CLI's own exit could in theory
		// reach a reused group id; pids are not reused that fast.)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = cliWaitDelay
	var stdout bytes.Buffer
	var log strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&log, jobLog)
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return cliRun{stdout: stdout.Bytes(), log: log.String(), err: err}
}

// withoutEnv returns env without the variables named.
func withoutEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		drop := false
		for _, n := range names {
			if name == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// cliVersionRE finds the CLI's version on the first line of its log
// ("@devcontainers/cli 0.89.0. Node.js v24.21.0. ...").
var cliVersionRE = regexp.MustCompile(`@devcontainers/cli (\d+\.\d+\.\d+)`)

// cliVersion is the CLI version a run's log names, or "".
func cliVersion(log string) string {
	if m := cliVersionRE.FindStringSubmatch(log); m != nil {
		return m[1]
	}
	return ""
}

// TestedCLIVersions are the devcontainer CLI versions whose docker calls and
// emitted docker run options fleet has recorded (the capture-signature
// goldens in testdata/signature). Another version still works as long as it
// emits nothing new; anything new fails the create with
// kubernetes:unsupported:flag, and CaptureUp warns that it is untested.
var TestedCLIVersions = []string{"0.85.0", "0.86.0", "0.86.1", "0.87.0", "0.88.0", "0.89.0"}

// Tested reports whether version is one of TestedCLIVersions.
func Tested(version string) bool {
	for _, v := range TestedCLIVersions {
		if v == version {
			return true
		}
	}
	return false
}

// untestedWarning is the warning for a CLI version outside the tested range.
func untestedWarning(version string) string {
	tested := TestedCLIVersions[0] + " to " + TestedCLIVersions[len(TestedCLIVersions)-1]
	if version == "" {
		return "could not read the devcontainer CLI's version; fleet has tested " + tested
	}
	return fmt.Sprintf("devcontainer CLI %s is not one fleet has tested (%s): a docker run option it emits that fleet does not know fails the create with kubernetes:unsupported:flag", version, tested)
}

// logTail is the end of a CLI log for an error message, environment values
// hidden (the CLI logs its docker run with every -e value).
func logTail(log string) string {
	const maxLines = 50
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return redact.EnvText(strings.TrimSpace(strings.Join(lines, "\n")))
}

// cliError is the error of a failed CLI run: the outcome message the CLI put
// on stdout, when it gave one, and the end of its log.
func cliError(what string, r cliRun) error {
	msg := fmt.Sprintf("%s failed: %v", what, r.err)
	if out := outcomeMessage(r.stdout); out != "" {
		msg += ": " + redact.EnvText(out)
	}
	if tail := logTail(r.log); tail != "" {
		msg += "\n" + tail
	}
	return errors.New(msg)
}

// outcome is the JSON result the CLI prints on stdout.
type outcome struct {
	Outcome     string `json:"outcome"`
	Message     string `json:"message"`
	Description string `json:"description"`
}

// outcomeMessage is the description and message of a failed run's outcome,
// or "" when stdout holds none.
func outcomeMessage(stdout []byte) string {
	var o outcome
	if json.Unmarshal(lastLine(stdout), &o) != nil || o.Outcome == "" || o.Outcome == "success" {
		return ""
	}
	switch {
	case o.Description != "" && o.Message != "":
		return o.Description + " " + o.Message
	case o.Description != "":
		return o.Description
	}
	return o.Message
}

// lastLine is the last non-empty line of out: the CLI prints its result last.
func lastLine(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	return lines[len(lines)-1]
}
