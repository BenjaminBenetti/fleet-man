package sshtunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hostkey.go handles the one ssh failure a human must decide: an UNKNOWN host
// key. ssh runs in batch mode with strict host-key checking, so a host that is
// not in known_hosts is refused — never silently trusted (no accept-new). When
// that happens the daemon fetches the host's public keys with ssh-keyscan,
// computes their SHA256 fingerprints with ssh-keygen -lf, and reports them as
// an UnknownHostKeyError so the client can show the fingerprint and ask. If
// the user accepts, TrustHostKey appends exactly the offered line to the
// daemon user's known_hosts (the first UserKnownHostsFile of the effective ssh
// config) and resolves again.
//
// A CHANGED key — the host is known but presents a different key — is the
// classic man-in-the-middle signal. It is never offered for acceptance: it
// surfaces as a ChangedHostKeyError naming the known_hosts file and line ssh
// reported, for the user to resolve by hand.

// keyscanTimeout bounds ssh-keyscan (its own -T is per-connection; this is the
// whole run) and the ssh -G / ssh-keygen helpers.
const keyscanTimeout = 20 * time.Second

// HostKey is one public key a host presented.
type HostKey struct {
	Type        string // known_hosts algorithm name: ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp256, …
	Fingerprint string // "SHA256:<base64>"
	Line        string // "<name> <type> <base64 blob>" — the exact known_hosts line
}

// UnknownHostKeyError reports that ssh refused a host whose key is not in
// known_hosts, together with what the host actually presented.
type UnknownHostKeyError struct {
	Target         Target
	Name           string    // the known_hosts lookup name ssh used ("host" or "[host]:port")
	Host           string    // hostname ssh-keyscan connected to (after ~/.ssh/config)
	Port           int       // port ssh-keyscan connected to
	KeyType        string    // the algorithm ssh wanted, as ssh spells it (ED25519, ECDSA, RSA)
	Keys           []HostKey // what the host offered; the KeyType match first
	KnownHostsPath string    // where TrustHostKey appends
}

func (e *UnknownHostKeyError) Error() string {
	summary := "host key for " + e.Name + " is not known"
	if k := e.primary(); k != nil {
		summary += fmt.Sprintf(" (%s %s)", k.Type, k.Fingerprint)
	}
	return summary + " — accept it in the fleet TUI (Settings → Fleet Armada, or switch to the remote), or run `ssh " + e.Target.destination() + "` once to trust it manually"
}

// primary is the key ssh would negotiate (the KeyType match), else the first.
func (e *UnknownHostKeyError) primary() *HostKey {
	if len(e.Keys) == 0 {
		return nil
	}
	return &e.Keys[0]
}

// ChangedHostKeyError reports that a KNOWN host presented a different key.
type ChangedHostKeyError struct {
	Name    string
	KeyType string
	File    string
	Line    int
}

func (e *ChangedHostKeyError) Error() string {
	where := e.File
	if e.Line > 0 {
		where = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	return fmt.Sprintf("host key for %s has CHANGED — the offending %s key is at %s. Refusing to connect: fleet never replaces a known host key. Verify the host, then fix that known_hosts line by hand", e.Name, e.KeyType, where)
}

var (
	// "No ED25519 host key is known for [127.0.0.1]:2222 and you have requested strict checking."
	reUnknownHostKey = regexp.MustCompile(`No (\S+) host key is known for (\S+) and you have requested strict checking\.`)
	// "Offending ECDSA key in /home/u/.ssh/known_hosts:3"
	reOffendingKey = regexp.MustCompile(`Offending (\S+) key in (.+?):(\d+)`)
	// "Host key for desktop has changed and you have requested strict checking."
	reChangedHost = regexp.MustCompile(`Host key for (\S+) has changed`)
)

// sshFailure is what classifySSHFailure recognised in ssh's stderr.
type sshFailure struct {
	unknown *struct{ keyType, name string }
	changed *ChangedHostKeyError
}

// classifySSHFailure recognises the two host-key outcomes in ssh's stderr;
// anything else is a generic failure (the caller shows the last line).
func classifySSHFailure(stderr string) sshFailure {
	var f sshFailure
	if m := reOffendingKey.FindStringSubmatch(stderr); m != nil {
		line, _ := strconv.Atoi(m[3])
		c := &ChangedHostKeyError{KeyType: m[1], File: m[2], Line: line}
		if h := reChangedHost.FindStringSubmatch(stderr); h != nil {
			c.Name = h[1]
		}
		f.changed = c
		return f
	}
	if m := reUnknownHostKey.FindStringSubmatch(stderr); m != nil {
		f.unknown = &struct{ keyType, name string }{m[1], m[2]}
	}
	return f
}

// hostKeyError turns a classified ssh failure into the structured error the
// caller should return, or nil when stderr showed neither host-key case. An
// unknown key is enriched by probing the host (keyscan + fingerprints); if
// that probe fails the error still says the key is unknown, plus why it could
// not be fetched.
func hostKeyError(ctx context.Context, t Target, stderr string) error {
	f := classifySSHFailure(stderr)
	switch {
	case f.changed != nil:
		if f.changed.Name == "" {
			f.changed.Name = t.destination()
		}
		return f.changed
	case f.unknown != nil:
		uk, err := probeHostKeys(ctx, t, f.unknown.keyType, f.unknown.name)
		if err != nil {
			return fmt.Errorf("host key for %s is not known, and fetching it failed: %w — run `ssh %s` once to trust it manually", f.unknown.name, err, t.destination())
		}
		return uk
	}
	return nil
}

// sshEffectiveConfig is what `ssh -G` reports for a target: where ssh really
// connects (HostName/Port from ~/.ssh/config may differ from the URL) and the
// known_hosts file it consults first.
type sshEffectiveConfig struct {
	hostname   string
	port       int
	knownHosts string
}

// resolveSSHConfig runs `ssh -G` with the production arguments, so aliases,
// Port, HostName and UserKnownHostsFile from the user's config all apply.
func resolveSSHConfig(ctx context.Context, t Target) (sshEffectiveConfig, error) {
	cctx, cancel := context.WithTimeout(ctx, keyscanTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ssh", append([]string{"-G"}, t.sshArgs()...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return sshEffectiveConfig{}, fmt.Errorf("ssh -G: %s", firstNonEmpty(lastLine(stderr.String()), err.Error()))
	}
	return parseSSHConfig(out.String())
}

// parseSSHConfig extracts hostname, port and the first UserKnownHostsFile
// from `ssh -G` output ("key value" lines).
func parseSSHConfig(out string) (sshEffectiveConfig, error) {
	var c sshEffectiveConfig
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "hostname":
			c.hostname = fields[1]
		case "port":
			c.port, _ = strconv.Atoi(fields[1])
		case "userknownhostsfile":
			c.knownHosts = expandHome(fields[1])
		}
	}
	if c.hostname == "" || c.port == 0 {
		return c, errors.New("ssh -G reported no hostname/port")
	}
	if c.knownHosts == "" {
		c.knownHosts = expandHome("~/.ssh/known_hosts")
	}
	return c, nil
}

// expandHome resolves a leading "~/" the way ssh does for UserKnownHostsFile.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// keyscan fetches the host's public keys with ssh-keyscan. Output lines are
// "<host or [host]:port> <type> <base64>"; comment lines (starting "#") go to
// stderr and are ignored.
func keyscan(ctx context.Context, host string, port int) ([]HostKey, error) {
	cctx, cancel := context.WithTimeout(ctx, keyscanTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ssh-keyscan", "-T", "5", "-p", strconv.Itoa(port), host)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	runErr := cmd.Run()
	keys := parseKeyscan(out.String())
	if len(keys) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("ssh-keyscan: %s", firstNonEmpty(lastLine(stderr.String()), runErr.Error()))
		}
		return nil, errors.New("ssh-keyscan returned no keys")
	}
	return keys, nil
}

// parseKeyscan reads ssh-keyscan's stdout into keys (Type + the raw blob kept
// in Line's last field; Fingerprint and the final Line are filled in later).
func parseKeyscan(out string) []HostKey {
	var keys []HostKey
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		keys = append(keys, HostKey{Type: fields[1], Line: fields[2]})
	}
	return keys
}

// fingerprint computes the SHA256 fingerprint of a public key with
// `ssh-keygen -lf` (the authoritative format users compare against).
func fingerprint(ctx context.Context, keyType, blob string) (string, error) {
	f, err := os.CreateTemp("", "fleet-hostkey-*.pub")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(keyType + " " + blob + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	cctx, cancel := context.WithTimeout(ctx, keyscanTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ssh-keygen", "-lf", f.Name()).Output()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen -lf: %w", err)
	}
	return parseFingerprint(string(out))
}

// parseFingerprint pulls "SHA256:…" out of ssh-keygen -l output
// ("256 SHA256:xxxx comment (ED25519)").
func parseFingerprint(out string) (string, error) {
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "SHA256:") {
			return field, nil
		}
	}
	return "", fmt.Errorf("no SHA256 fingerprint in ssh-keygen output %q", strings.TrimSpace(out))
}

// probeHostKeys builds the UnknownHostKeyError for a refused host: resolves
// the effective ssh config, keyscans the real host:port, fingerprints every
// key, and composes the known_hosts lines under the name ssh will look up.
func probeHostKeys(ctx context.Context, t Target, wantType, name string) (*UnknownHostKeyError, error) {
	cfg, err := resolveSSHConfig(ctx, t)
	if err != nil {
		return nil, err
	}
	raw, err := keyscan(ctx, cfg.hostname, cfg.port)
	if err != nil {
		return nil, err
	}
	uk := &UnknownHostKeyError{Target: t, Name: name, Host: cfg.hostname, Port: cfg.port, KeyType: wantType, KnownHostsPath: cfg.knownHosts}
	for _, k := range raw {
		fp, err := fingerprint(ctx, k.Type, k.Line)
		if err != nil {
			return nil, err
		}
		hk := HostKey{Type: k.Type, Fingerprint: fp, Line: name + " " + k.Type + " " + k.Line}
		if keyTypeMatches(wantType, k.Type) {
			uk.Keys = append([]HostKey{hk}, uk.Keys...)
		} else {
			uk.Keys = append(uk.Keys, hk)
		}
	}
	return uk, nil
}

// keyTypeMatches relates ssh's spelling in its error ("ED25519", "ECDSA",
// "RSA") to a known_hosts algorithm name ("ssh-ed25519", "ecdsa-sha2-nistp256",
// "ssh-rsa").
func keyTypeMatches(want, algo string) bool {
	w := strings.ToLower(want)
	a := strings.ToLower(algo)
	switch {
	case strings.HasPrefix(w, "ed25519"):
		return strings.HasPrefix(a, "ssh-ed25519")
	case strings.HasPrefix(w, "ecdsa"):
		return strings.HasPrefix(a, "ecdsa-")
	case strings.HasPrefix(w, "rsa"):
		return strings.HasPrefix(a, "ssh-rsa")
	case strings.HasPrefix(w, "dsa"):
		return strings.HasPrefix(a, "ssh-dss")
	}
	return strings.Contains(a, w)
}

// validKnownHostsLine checks the shape of a line before it is appended:
// exactly "<name> <type> <base64>" with no control characters (a line break
// could smuggle in a second entry).
func validKnownHostsLine(line string) bool {
	if strings.ContainsAny(line, "\r\n\x00") {
		return false
	}
	return len(strings.Fields(line)) == 3
}

// appendKnownHosts adds line to path: the directory is created 0700 and the
// file 0600 when missing, existing lines are never touched, a missing final
// newline is repaired first, and a line already present is not duplicated.
func appendKnownHosts(path, line string) error {
	if !validKnownHostsLine(line) {
		return fmt.Errorf("refusing to write malformed known_hosts line %q", line)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == line {
			return nil // already trusted
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
