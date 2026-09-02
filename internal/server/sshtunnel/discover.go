package sshtunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// discover.go learns what to tunnel to: the remote daemon's SSH-mode gRPC port
// and its bearer token, both plain files under the remote ~/.fleet. It runs a
// small POSIX sh script over ssh (fed on stdin, so the remote login shell's
// flavour doesn't matter and nothing has to be quoted) rather than a remote
// `fleet` command — a non-interactive ssh gets no ~/.bashrc, so `fleet` is
// often not on PATH. The script does try to START the remote daemon when it is
// down (the port file is a liveness hint, removed on shutdown) by running a
// cheap client command through the usual auto-spawn path, probing the common
// install locations.

// discoverTimeout bounds one discovery round trip: ssh connect + auth, plus a
// possible daemon auto-spawn (the script waits up to ~10s for it).
const discoverTimeout = 45 * time.Second

// discoverScript is the remote-side probe. Its single stdout result line is one
// of:
//
//	FLEET_OK <port> <token>   listener up; tunnel to <port>, auth with <token>
//	FLEET_SSH_OFF             daemon running but Remote Fleet via SSH is off
//	FLEET_NO_DAEMON           no daemon and none could be started (fleet not
//	                          installed, or the client command failed)
//
// ssh.port is written when the SSH listener comes up and removed when it stops
// or the daemon exits; server.version is written at the END of daemon startup
// (after the listener converged) and removed on exit, so once it exists the
// port file is either there or never will be — the wait loop keys on both.
const discoverScript = `d="$HOME/.fleet"
if [ ! -s "$d/ssh.port" ] && [ ! -e "$d/server.version" ]; then
  started=
  for c in "$(command -v fleet 2>/dev/null)" "$HOME/.local/bin/fleet" "$HOME/go/bin/fleet" /usr/local/bin/fleet /opt/homebrew/bin/fleet; do
    if [ -n "$c" ] && [ -x "$c" ]; then
      if "$c" list >/dev/null 2>&1 </dev/null; then started=1; fi
      break
    fi
  done
  if [ -n "$started" ]; then
    i=0
    while [ ! -s "$d/ssh.port" ] && [ ! -e "$d/server.version" ] && [ "$i" -lt 10 ]; do
      sleep 1
      i=$((i+1))
    done
  fi
fi
if [ -s "$d/ssh.port" ] && [ -s "$d/mcp.token" ]; then
  printf 'FLEET_OK %s %s\n' "$(cat "$d/ssh.port")" "$(cat "$d/mcp.token")"
elif [ -e "$d/server.version" ]; then
  echo FLEET_SSH_OFF
else
  echo FLEET_NO_DAEMON
fi
`

// Discovery is what the remote reported: the loopback port its SSH-mode gRPC
// listener is bound to and the bearer token it expects.
type Discovery struct {
	Port  int
	Token string
}

// discoverOverSSH runs discoverScript on the target and parses its result.
func discoverOverSSH(ctx context.Context, t Target) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", t.sshArgs()...)
	cmd.Args = append(cmd.Args, "sh")
	cmd.Stdin = strings.NewReader(discoverScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	setSysProcAttr(cmd)
	runErr := cmd.Run()
	d, parseErr := parseDiscovery(stdout.String())
	if parseErr == nil {
		return d, nil
	}
	if ctx.Err() != nil {
		return Discovery{}, fmt.Errorf("ssh timed out after %s", discoverTimeout)
	}
	if runErr != nil {
		// ssh itself failed (auth, host key, unreachable): its stderr says why.
		if line := lastLine(stderr.String()); line != "" {
			return Discovery{}, fmt.Errorf("ssh: %s", line)
		}
		return Discovery{}, fmt.Errorf("ssh: %w", runErr)
	}
	return Discovery{}, parseErr
}

// Sentinel errors for the two "ssh worked, but no listener" outcomes, so callers
// can word them for the user (they name the host).
var (
	ErrSSHModeOff = errors.New("Remote Fleet via SSH is not enabled on the remote (Settings → Fleet MCP), or its fleet is too old")
	ErrNoDaemon   = errors.New("no fleet daemon is running on the remote and none could be started (is fleet installed there?)")
)

// parseDiscovery finds the FLEET_* result line in the script's stdout (scanning
// every line, so a chatty remote shell profile can't break it).
func parseDiscovery(out string) (Discovery, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "FLEET_OK "):
			fields := strings.Fields(line)
			if len(fields) != 3 {
				return Discovery{}, fmt.Errorf("malformed discovery reply: %q", line)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil || port <= 0 || port > 65535 {
				return Discovery{}, fmt.Errorf("malformed discovery port: %q", fields[1])
			}
			return Discovery{Port: port, Token: fields[2]}, nil
		case line == "FLEET_SSH_OFF":
			return Discovery{}, ErrSSHModeOff
		case line == "FLEET_NO_DAEMON":
			return Discovery{}, ErrNoDaemon
		}
	}
	return Discovery{}, errors.New("no discovery reply from the remote (is `sh` available there?)")
}

// lastLine returns the last non-empty line of s, trimmed — ssh's diagnostic is
// normally its final line ("Permission denied (publickey).", "Host key
// verification failed.", "connect to host …: Connection refused").
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
