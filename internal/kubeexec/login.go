//go:build !nok8s

package kubeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/rest"
)

// login.go runs a connection's exec credential plugin as a login: once, with
// the ExecCredential request marked interactive, so a plugin that signs in
// through a device code (kubelogin, an SSO helper) does so instead of
// failing. The daemon has no terminal, so the user is wherever the client is:
// the plugin's stderr is read line by line and only the device-code URL and
// code in it are passed on (DeviceCode). Nothing else of its output leaves
// the daemon; its stdout holds the credential.

// LoginTimeout bounds a login run when the caller's context does not.
const LoginTimeout = 10 * time.Minute

// DeviceCode is what a login plugin asks the user to do: open URL and enter
// Code. Either may be empty while the plugin has printed only the other.
type DeviceCode struct {
	URL  string
	Code string
}

// ErrNoLoginPlugin: the connection does not authenticate through an exec
// credential plugin, so there is nothing to log in with.
var ErrNoLoginPlugin = errors.New("the connection does not use an exec credential plugin")

// Login runs the connection's credential plugin interactively and returns
// the credential it produced (never its secret). Each device code the plugin
// prints is passed to onCode, in order, once. A failing plugin returns a
// *PluginError whose message is safe to relay.
func (c *Connection) Login(ctx context.Context, onCode func(DeviceCode)) (Credential, error) {
	if c.Auth != AuthExecPlugin || c.Plugin == nil {
		return Credential{Auth: c.Auth}, ErrNoLoginPlugin
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, LoginTimeout)
		defer cancel()
	}
	return c.Plugin.login(ctx, onCode)
}

// LoginCommand is the connection's credential plugin set up to log in in
// the caller's own terminal: the TUI's "Run login here", offered only over
// the local socket, where the caller's machine is the daemon host. The
// ExecCredential request is marked interactive and the environment is the one
// fleetd gives the plugin (built from the caller's), so a sign-in it completes
// lands in the same token caches fleetd's own runs read. Stdin and stderr are
// left unset for the caller to connect to the terminal; stdout carries the
// credential, so it goes nowhere and is never shown.
func (c *Connection) LoginCommand() (*exec.Cmd, error) {
	if c.Auth != AuthExecPlugin || c.Plugin == nil {
		return nil, ErrNoLoginPlugin
	}
	info, err := c.Plugin.loginExecInfo()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Plugin.Path, c.Plugin.Args...)
	cmd.Env = c.Plugin.environ(info)
	cmd.Stdout = io.Discard
	return cmd, nil
}

func (p *Plugin) login(ctx context.Context, onCode func(DeviceCode)) (Credential, error) {
	info, err := p.loginExecInfo()
	if err != nil {
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, err
	}
	cmd := exec.CommandContext(ctx, p.Path, p.Args...)
	cmd.Env = p.environ(info)
	cmd.Stdin = nil
	stdout := &cappedBuffer{max: maxPluginStdout}
	stderr := newDeviceCodeWriter(onCode)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = pluginWaitDelay
	ownProcessGroup(cmd)
	runErr := cmd.Run()
	stderr.flush()
	if runErr != nil {
		perr := &PluginError{Path: p.Path, Stderr: SanitizeOutput(stderr.tail()), Err: runErr}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			perr.ExitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			perr.Err = ctx.Err()
		}
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, perr
	}
	if stdout.truncated {
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, &PluginError{Path: p.Path, Err: fmt.Errorf("its output is over %d bytes", maxPluginStdout)}
	}
	cred, err := p.decode(stdout.Bytes())
	if err != nil {
		return Credential{Auth: AuthExecPlugin, Refreshable: true}, &PluginError{Path: p.Path, Err: err}
	}
	got := Credential{Auth: AuthExecPlugin, Refreshable: true}
	if exp := cred.Status.ExpirationTimestamp; exp != nil {
		got.ExpiresAt = exp.Time
	}
	return got, nil
}

// loginExecInfo is execInfo with the request marked interactive.
func (p *Plugin) loginExecInfo() ([]byte, error) {
	gv, err := p.groupVersion()
	if err != nil {
		return nil, err
	}
	cred := &clientauthentication.ExecCredential{Spec: clientauthentication.ExecCredentialSpec{Interactive: true}}
	if p.provideClusterInfo {
		cluster, err := rest.ConfigToExecCluster(p.restConfig)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMisconfigured, err)
		}
		cred.Spec.Cluster = cluster
	}
	return runtime.Encode(authCodecFactory().LegacyCodec(gv), cred)
}

// deviceCodeWriter is a login plugin's stderr: it splits the output into
// lines, keeps the last few KiB for an error message, and reports each new
// device code it finds in them.
type deviceCodeWriter struct {
	mu      sync.Mutex
	onCode  func(DeviceCode)
	partial []byte
	last    []byte // the most recent output, at most maxLoginTail bytes
	total   int
	parser  DeviceCodeParser
	sent    DeviceCode
	reports int
}

const (
	// maxLoginStderr is how much of a login's stderr is examined at all.
	maxLoginStderr = maxPluginStderr
	maxLoginTail   = 4 << 10
	// maxDeviceCodes caps the codes one login relays.
	maxDeviceCodes = 16
)

func newDeviceCodeWriter(onCode func(DeviceCode)) *deviceCodeWriter {
	return &deviceCodeWriter{onCode: onCode}
}

func (w *deviceCodeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.last = append(w.last, p...)
	if len(w.last) > maxLoginTail {
		w.last = append([]byte(nil), w.last[len(w.last)-maxLoginTail:]...)
	}
	if room := maxLoginStderr - w.total; room <= 0 {
		return n, nil
	} else if len(p) > room {
		p = p[:room]
	}
	w.total += len(p)
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexAny(w.partial, "\r\n")
		if i < 0 {
			break
		}
		w.line(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	return n, nil
}

func (w *deviceCodeWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.line(string(w.partial))
		w.partial = nil
	}
}

func (w *deviceCodeWriter) tail() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.last...)
}

func (w *deviceCodeWriter) line(s string) {
	got, ok := w.parser.Line(s)
	if !ok || got == w.sent || w.reports >= maxDeviceCodes || w.onCode == nil {
		return
	}
	w.sent = got
	w.reports++
	w.onCode(got)
}

// DeviceCodeParser picks the device-code URL and code out of a login
// plugin's output, one line at a time. The zero value is ready to use.
type DeviceCodeParser struct {
	cur DeviceCode
	// wantCode: the last line announced a code without one ("Then enter the
	// code:"), so a line holding only a code is it.
	wantCode bool
}

var (
	urlPattern = regexp.MustCompile(`https?://[^\s"'<>()\[\]{}]+`)
	// codeAfterWord finds a code right after the word "code" ("enter the
	// code ABCD-EFGH", "user code: ABCD1234").
	codeAfterWord = regexp.MustCompile(`(?i)\bcode\b[\s:"'=]*(?:is\s+)?["']?([A-Za-z0-9]{4,12}(?:-[A-Za-z0-9]{4,12})?)\b`)
	// codeAlone is a line that is only a code.
	codeAlone = regexp.MustCompile(`^["']?([A-Z0-9]{4,12}(?:-[A-Z0-9]{4,12})?)["']?$`)
	codeWord  = regexp.MustCompile(`(?i)\bcode\b`)
)

// Line reads one line of output and returns the device code seen so far,
// and whether this line changed it.
func (d *DeviceCodeParser) Line(line string) (DeviceCode, bool) {
	text := SanitizeOutput([]byte(line))
	if text == "" {
		return d.cur, false
	}
	before := d.cur
	if u := urlPattern.FindString(text); u != "" {
		d.cur.URL = strings.TrimRight(u, ".,;:!?")
	}
	withoutURL := urlPattern.ReplaceAllString(text, " ")
	switch m := codeAfterWord.FindStringSubmatch(withoutURL); {
	case m != nil && looksLikeCode(m[1]):
		d.cur.Code = m[1]
		d.wantCode = false
	case d.wantCode && codeAlone.MatchString(strings.TrimSpace(withoutURL)):
		d.cur.Code = codeAlone.FindStringSubmatch(strings.TrimSpace(withoutURL))[1]
		d.wantCode = false
	case codeWord.MatchString(withoutURL):
		d.wantCode = true
	}
	return d.cur, d.cur != before
}

// looksLikeCode tells a device code from an ordinary word that follows
// "code" ("the code to authenticate"): codes carry a digit, a dash or are
// upper case.
func looksLikeCode(s string) bool {
	if strings.ContainsAny(s, "-0123456789") {
		return true
	}
	return s == strings.ToUpper(s)
}
