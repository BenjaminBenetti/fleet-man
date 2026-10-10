//go:build !nok8s

package kubeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// basePluginEnv are the daemon environment variables every credential plugin
// gets: what any CLI needs to run (home, user, temp dir, time zone), locale
// (and every LC_*), proxies and TLS trust. Anything else of the daemon's
// environment reaches a plugin only when the record's plugin_env names it.
var basePluginEnv = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "TZ": true,
	"LANG": true, "LANGUAGE": true,
	"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

func isBasePluginEnv(name string) bool {
	return basePluginEnv[name] || strings.HasPrefix(name, "LC_")
}

// execInfoEnv is the variable client-go hands a plugin its ExecCredential
// request in.
const execInfoEnv = "KUBERNETES_EXEC_INFO"

// Plugin is a context's exec credential plugin as fleet runs it.
type Plugin struct {
	// Command is what the kubeconfig names; Path is the absolute file it
	// resolved to through the record's extra_path and the daemon's PATH.
	Command string
	Path    string
	Args    []string
	// Env is the plugin's whole environment ("NAME=value", sorted). It can
	// hold credentials passed through plugin_env: never log it.
	Env []string
	// APIVersion is the client.authentication.k8s.io version it speaks.
	APIVersion  string
	InstallHint string

	provideClusterInfo bool
	restConfig         *rest.Config // for KUBERNETES_EXEC_INFO's cluster
}

// newPlugin resolves cfg's exec plugin for record c.
func newPlugin(cfg *rest.Config, c fleet.KubeCluster, environ []string) (*Plugin, error) {
	ec := cfg.ExecProvider
	env := pluginEnv(environ, ec.Env, c.PluginEnv, c.ExtraPath)
	path, err := resolvePlugin(ec.Command, envMap(env)["PATH"])
	if err != nil {
		hint := ""
		if h := SanitizeOutput([]byte(ec.InstallHint)); h != "" {
			hint = "; " + h
		}
		return nil, fmt.Errorf("%w: %v%s", ErrPluginMissing, err, hint)
	}
	return &Plugin{
		Command:            ec.Command,
		Path:               path,
		Args:               append([]string(nil), ec.Args...),
		Env:                env,
		APIVersion:         ec.APIVersion,
		InstallHint:        ec.InstallHint,
		provideClusterInfo: ec.ProvideClusterInfo,
		restConfig:         rest.CopyConfig(cfg),
	}, nil
}

// pluginEnv is a plugin's environment: the base daemon variables, then the
// kubeconfig's own env entries, then plugin_env ("NAME" passes the daemon's
// value through, "NAME=value" sets it), and PATH: extraPath in front of the
// kubeconfig's PATH entry, else the daemon's.
func pluginEnv(environ []string, kubeconfigEnv []clientcmdapi.ExecEnvVar, recordEnv, extraPath []string) []string {
	daemon := envMap(environ)
	out := map[string]string{}
	for name, value := range daemon {
		if isBasePluginEnv(name) {
			out[name] = value
		}
	}
	path, pathSet := daemon["PATH"]
	for _, e := range kubeconfigEnv {
		if e.Name == "PATH" {
			path, pathSet = e.Value, true
			continue
		}
		out[e.Name] = e.Value
	}
	for _, entry := range recordEnv {
		name, value, hasValue := fleet.ParsePluginEnv(entry)
		if !hasValue {
			var ok bool
			if value, ok = daemon[name]; !ok {
				continue
			}
		}
		out[name] = value
	}
	dirs := append([]string(nil), extraPath...)
	if pathSet {
		dirs = append(dirs, splitList(path)...)
	}
	if len(dirs) > 0 {
		out["PATH"] = strings.Join(dirs, string(filepath.ListSeparator))
	}
	env := make([]string, 0, len(out))
	for _, name := range sortedKeys(out) {
		env = append(env, name+"="+out[name])
	}
	return env
}

// resolvePlugin finds the plugin file: an absolute command as it is, a bare
// name through path's absolute entries (a relative entry, such as ".", is
// never searched).
func resolvePlugin(command, path string) (string, error) {
	if command == "" {
		return "", errors.New("the kubeconfig's exec entry has no command")
	}
	if strings.ContainsRune(command, filepath.Separator) {
		if !filepath.IsAbs(command) {
			return "", fmt.Errorf("plugin %q is a relative path", command)
		}
		if err := checkExecutable(command); err != nil {
			return "", fmt.Errorf("plugin %s: %w", command, err)
		}
		return filepath.Clean(command), nil
	}
	for _, dir := range splitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, command)
		if checkExecutable(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("plugin %q is not on the daemon host's PATH (%s); add its directory to the connection's extra_path", command, path)
}

func checkExecutable(path string) error {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return errors.New("not a regular file")
	case info.Mode().Perm()&0o111 == 0:
		return errors.New("not executable")
	}
	return nil
}

// execConfig is the exec provider client-go runs the plugin with: the
// resolved path (and only that path, by plugin policy), never interactive,
// with the plugin's environment. client-go starts plugins with the daemon's
// whole environment and appends the provider's entries, the last value of a
// name winning; so every other daemon variable is set empty here, and a
// plugin sees nothing of the daemon's environment beyond Env.
func (p *Plugin) execConfig(environ []string) *clientcmdapi.ExecConfig {
	ec := p.restConfig.ExecProvider.DeepCopy()
	ec.Command = p.Path
	ec.Args = append([]string(nil), p.Args...)
	ec.InteractiveMode = clientcmdapi.NeverExecInteractiveMode
	ec.StdinUnavailable = true
	ec.StdinUnavailableMessage = "fleet runs credential plugins without a terminal"
	ec.PluginPolicy = clientcmdapi.PluginPolicy{
		PolicyType: clientcmdapi.PluginPolicyAllowlist,
		Allowlist:  []clientcmdapi.AllowlistEntry{{Name: p.Path}},
	}
	kept := envMap(p.Env)
	ec.Env = nil
	for _, kv := range p.Env {
		name, value, _ := strings.Cut(kv, "=")
		ec.Env = append(ec.Env, clientcmdapi.ExecEnvVar{Name: name, Value: value})
	}
	for _, name := range sortedKeys(envMap(environ)) {
		if _, ok := kept[name]; !ok && name != execInfoEnv {
			ec.Env = append(ec.Env, clientcmdapi.ExecEnvVar{Name: name})
		}
	}
	return ec
}

// --- running the plugin ---

// Output caps for a plugin run.
const (
	maxPluginStdout = 1 << 20
	maxPluginStderr = 64 << 10
	// MaxPluginMessage is the most of a plugin's stderr an error carries.
	MaxPluginMessage = 512
	pluginWaitDelay  = 2 * time.Second
)

// PluginError is a plugin run that failed. Stderr is sanitized and at most
// MaxPluginMessage bytes, safe to show to any client.
type PluginError struct {
	Path     string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *PluginError) Error() string {
	msg := fmt.Sprintf("credential plugin %s", e.Path)
	if e.ExitCode > 0 {
		msg += fmt.Sprintf(" exited %d", e.ExitCode)
	} else if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *PluginError) Unwrap() error { return e.Err }

// run runs the plugin once, non-interactively, as client-go would, and
// returns its stdout. ctx bounds it.
func (p *Plugin) run(ctx context.Context) ([]byte, error) {
	info, err := p.execInfo()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, p.Path, p.Args...)
	cmd.Env = append(append([]string(nil), p.Env...), execInfoEnv+"="+string(info))
	cmd.Stdin = nil
	stdout := &cappedBuffer{max: maxPluginStdout}
	stderr := &cappedBuffer{max: maxPluginStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = pluginWaitDelay
	ownProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		perr := &PluginError{Path: p.Path, Stderr: SanitizeOutput(stderr.Bytes()), Err: err}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			perr.ExitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			perr.Err = ctx.Err()
		}
		return nil, perr
	}
	if stdout.truncated {
		return nil, &PluginError{Path: p.Path, Err: fmt.Errorf("its output is over %d bytes", maxPluginStdout)}
	}
	return stdout.Bytes(), nil
}

// cappedBuffer keeps the first max bytes written and drops the rest, so a
// runaway plugin cannot grow the daemon's memory.
type cappedBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*cappedBuffer)(nil)

// SanitizeOutput makes a plugin's output safe to relay to a client: escape
// sequences and control characters (but newlines and tabs) are dropped,
// invalid UTF-8 is replaced, and the text is cut to MaxPluginMessage bytes.
func SanitizeOutput(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\uFFFD")
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC: drop the whole escape sequence
			i = skipEscape(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) {
			continue
		}
		out.WriteRune(r)
	}
	text := strings.TrimSpace(out.String())
	if len(text) > MaxPluginMessage {
		cut := MaxPluginMessage - len("…")
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// skipEscape returns the index after the escape sequence starting at s[i].
func skipEscape(s string, i int) int {
	i++ // ESC
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameters, then a final byte in 0x40-0x7e
		for i++; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return i
	case ']': // OSC: up to BEL or ST (ESC \)
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return i
	}
	return i + 1 // a two-byte escape
}

// --- environment helpers ---

// envMap parses "NAME=value" entries; for a repeated name the last wins, as
// in os/exec.
func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && name != "" {
			m[name] = value
		}
	}
	return m
}

func splitList(list string) []string {
	var out []string
	for _, p := range filepath.SplitList(list) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
