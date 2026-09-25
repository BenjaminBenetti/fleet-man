package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
)

// HostKeyPrompt returns the exact line a human accepted, or an empty line to
// decline. It runs on the daemon; the implementation asks a connected client.
type HostKeyPrompt func(context.Context, string, *sshtunnel.UnknownHostKeyError) (string, error)

type hostKeyPromptContextKey struct{}

// WithHostKeyPrompt carries the interactive client for a provisioning request.
// A background task without one retains the ordinary clone failure and hint.
func WithHostKeyPrompt(ctx context.Context, prompt HostKeyPrompt) context.Context {
	return context.WithValue(ctx, hostKeyPromptContextKey{}, prompt)
}

// Clone runs git with strict, noninteractive OpenSSH defaults, preserving an
// explicit SSH command. On an unknown key it asks the request's client, saves
// exactly the accepted key on this host, and retries once. Git removes its own
// incomplete clone on a transport failure; we never delete a destination.
func Clone(ctx context.Context, remote string, args []string, output io.Writer) ([]byte, error) {
	managedSSH := defaultCloneSSH(ctx)
	run := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		if managedSSH {
			cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o CheckHostIP=no -o ConnectTimeout=15")
		}
		var buf bytes.Buffer
		var writer io.Writer = &buf
		if output != nil {
			writer = io.MultiWriter(output, &buf)
		}
		cmd.Stdout, cmd.Stderr = writer, writer
		err := cmd.Run()
		return buf.Bytes(), err
	}
	out, err := run()
	prompt, _ := ctx.Value(hostKeyPromptContextKey{}).(HostKeyPrompt)
	if err == nil || !managedSSH || prompt == nil || ctx.Err() != nil || !strings.Contains(string(out), "Host key verification failed") {
		return out, err
	}
	// Resolve insteadOf rewrites using git itself, without contacting the host.
	// Otherwise a github.com shorthand could prompt for the wrong server.
	resolved, resolveErr := exec.CommandContext(ctx, "git", "ls-remote", "--get-url", "--", remote).Output()
	if resolveErr != nil {
		return out, err
	}
	target, ok := cloneSSHTarget(strings.TrimSpace(string(resolved)))
	if !ok {
		return out, err
	}
	offer, probeErr := sshtunnel.ProbeHostKey(ctx, target, string(out))
	if probeErr != nil || offer == nil {
		return out, err
	}
	line, promptErr := prompt(ctx, remote, offer)
	if promptErr != nil {
		return out, fmt.Errorf("%w (host-key approval: %v)", err, promptErr)
	}
	if line == "" || ctx.Err() != nil {
		return out, err
	}
	if trustErr := sshtunnel.TrustOfferedHostKey(offer, line); trustErr != nil {
		return out, fmt.Errorf("%w (trust host key: %v)", err, trustErr)
	}
	return run()
}

// A custom transport may reach another host or use another known_hosts file.
// Leave it intact and never probe/record keys using assumptions about ssh.
func defaultCloneSSH(ctx context.Context) bool {
	for _, name := range []string{"GIT_SSH_COMMAND", "GIT_SSH"} {
		if _, set := os.LookupEnv(name); set {
			return false
		}
	}
	if variant := os.Getenv("GIT_SSH_VARIANT"); variant != "" && variant != "ssh" {
		return false
	}
	err := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand").Run()
	// Exit 1 means absent. Other failures must not enable an override.
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode() == 1
	}
	return false
}

func cloneSSHTarget(remote string) (sshtunnel.Target, bool) {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || (u.Scheme != "ssh" && u.Scheme != "git+ssh" && u.Scheme != "ssh+git") {
			return sshtunnel.Target{}, false
		}
		u.Scheme, u.Path, u.RawPath, u.RawQuery, u.Fragment = "ssh", "", "", "", ""
		t, err := sshtunnel.ParseURL(u.String())
		return t, err == nil
	}
	// The colon outside IPv6 brackets separates a scp-style path. A slash
	// before it makes this a local path, as in git's own URL parser.
	bracket := false
	for i, c := range remote {
		switch c {
		case '/':
			return sshtunnel.Target{}, false
		case '[':
			bracket = true
		case ']':
			bracket = false
		case ':':
			if !bracket {
				t, err := sshtunnel.ParseURL("ssh://" + remote[:i])
				return t, err == nil
			}
		}
	}
	return sshtunnel.Target{}, false
}
