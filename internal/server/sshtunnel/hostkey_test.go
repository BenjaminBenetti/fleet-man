package sshtunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const stderrUnknown = `Warning: Permanently added is NOT happening here
No ED25519 host key is known for [127.0.0.1]:2222 and you have requested strict checking.
Host key verification failed.
`

const stderrChanged = `@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!
The fingerprint for the ECDSA key sent by the remote host is
SHA256:AbCdEf.
Please contact your system administrator.
Add correct host key in /home/ben/.ssh/known_hosts to get rid of this message.
Offending ECDSA key in /home/ben/.ssh/known_hosts:3
Host key for desktop has changed and you have requested strict checking.
Host key verification failed.
`

func TestClassifySSHFailure(t *testing.T) {
	f := classifySSHFailure(stderrUnknown)
	if f.unknown == nil || f.changed != nil || f.unknown.keyType != "ED25519" || f.unknown.name != "[127.0.0.1]:2222" {
		t.Fatalf("unknown: %+v", f)
	}
	f = classifySSHFailure(stderrChanged)
	if f.changed == nil || f.unknown != nil {
		t.Fatalf("changed: %+v", f)
	}
	if f.changed.KeyType != "ECDSA" || f.changed.File != "/home/ben/.ssh/known_hosts" || f.changed.Line != 3 || f.changed.Name != "desktop" {
		t.Fatalf("changed fields: %+v", f.changed)
	}
	if f := classifySSHFailure("ben@desktop: Permission denied (publickey).\n"); f.unknown != nil || f.changed != nil {
		t.Fatalf("auth failure must not classify as a host-key case: %+v", f)
	}
	// A changed key never becomes an "unknown" offer, even if both phrases appear.
	if f := classifySSHFailure(stderrChanged + stderrUnknown); f.changed == nil || f.unknown != nil {
		t.Fatalf("changed must win over unknown: %+v", f)
	}
}

func TestChangedHostKeyErrorNamesFileAndLine(t *testing.T) {
	err := hostKeyError(context.Background(), Target{Host: "desktop"}, stderrChanged)
	var c *ChangedHostKeyError
	if !errors.As(err, &c) {
		t.Fatalf("want ChangedHostKeyError, got %T %v", err, err)
	}
	msg := err.Error()
	for _, want := range []string{"desktop", "CHANGED", "ECDSA", "/home/ben/.ssh/known_hosts:3", "never replaces", "by hand"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if hostKeyError(context.Background(), Target{Host: "h"}, "Permission denied (publickey).") != nil {
		t.Fatal("a non-host-key failure must classify as nil")
	}
}

func TestParseKeyscan(t *testing.T) {
	out := "# desktop:22 SSH-2.0-OpenSSH_9.2\n" +
		"[127.0.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample\n" +
		"[127.0.0.1]:2222 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQ\n" +
		"\n" +
		"garbage line\n"
	keys := parseKeyscan(out)
	if len(keys) != 2 || keys[0].Type != "ssh-ed25519" || keys[0].Line != "AAAAC3NzaC1lZDI1NTE5AAAAIExample" || keys[1].Type != "ssh-rsa" {
		t.Fatalf("parseKeyscan = %+v", keys)
	}
}

func TestParseSSHConfig(t *testing.T) {
	out := "user ben\nhostname 10.0.0.5\nport 2222\nuserknownhostsfile ~/.ssh/known_hosts ~/.ssh/known_hosts2\nstricthostkeychecking true\n"
	c, err := parseSSHConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.hostname != "10.0.0.5" || c.port != 2222 || c.knownHosts != filepath.Join(home, ".ssh/known_hosts") {
		t.Fatalf("parseSSHConfig = %+v", c)
	}
	if _, err := parseSSHConfig("user ben\n"); err == nil {
		t.Fatal("missing hostname/port must be an error")
	}
	c, _ = parseSSHConfig("hostname h\nport 22\nuserknownhostsfile /tmp/kh\n")
	if c.knownHosts != "/tmp/kh" {
		t.Fatalf("absolute known_hosts kept: %q", c.knownHosts)
	}
}

func TestParseFingerprint(t *testing.T) {
	fp, err := parseFingerprint("256 SHA256:pE3tP2y6ZlqXbVRFzM4W2wsdb2H1XUoP6Yt9j1cSE7M no comment (ED25519)\n")
	if err != nil || fp != "SHA256:pE3tP2y6ZlqXbVRFzM4W2wsdb2H1XUoP6Yt9j1cSE7M" {
		t.Fatalf("parseFingerprint = %q, %v", fp, err)
	}
	if _, err := parseFingerprint("garbage"); err == nil {
		t.Fatal("no SHA256 field must be an error")
	}
}

// TestFingerprintMatchesGo cross-checks ssh-keygen -lf against x/crypto's
// SHA256 fingerprint of a freshly generated key.
func TestFingerprintMatchesGo(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	blob := base64.StdEncoding.EncodeToString(sshPub.Marshal())
	got, err := fingerprint(context.Background(), sshPub.Type(), blob)
	if err != nil {
		t.Fatal(err)
	}
	if want := ssh.FingerprintSHA256(sshPub); got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}

func TestKeyTypeMatches(t *testing.T) {
	cases := []struct {
		want, algo string
		ok         bool
	}{
		{"ED25519", "ssh-ed25519", true}, {"ECDSA", "ecdsa-sha2-nistp256", true}, {"RSA", "ssh-rsa", true},
		{"ED25519", "ssh-rsa", false}, {"RSA", "ecdsa-sha2-nistp256", false},
	}
	for _, c := range cases {
		if keyTypeMatches(c.want, c.algo) != c.ok {
			t.Errorf("keyTypeMatches(%s, %s) != %v", c.want, c.algo, c.ok)
		}
	}
}

func TestAppendKnownHosts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ssh")
	path := filepath.Join(dir, "known_hosts")
	line := "[127.0.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample"

	// Missing dir + file: created with 0700 / 0600.
	if err := appendKnownHosts(path, line); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", st.Mode(), err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st.Mode(), err)
	}
	if got, _ := os.ReadFile(path); string(got) != line+"\n" {
		t.Fatalf("content = %q", got)
	}

	// Existing lines are kept; a missing final newline is repaired; duplicates skipped.
	if err := os.WriteFile(path, []byte("other ssh-rsa AAAA\n"+line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendKnownHosts(path, line); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "other ssh-rsa AAAA\n"+line {
		t.Fatalf("duplicate line appended: %q", got)
	}
	if err := appendKnownHosts(path, "new ssh-rsa BBBB"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "other ssh-rsa AAAA\n"+line+"\nnew ssh-rsa BBBB\n" {
		t.Fatalf("append with newline repair: %q", got)
	}

	// Malformed lines are refused.
	for _, bad := range []string{"", "only two", "a b c\nd e f", "a b c d"} {
		if err := appendKnownHosts(path, bad); err == nil {
			t.Errorf("appendKnownHosts(%q) should be refused", bad)
		}
	}
}

// TestTrustHostKeyRefusesUnofferedLine: only a line the daemon itself offered
// for that remote may be written, and an offer is consumed by one accept.
func TestTrustHostKeyRefusesUnofferedLine(t *testing.T) {
	m := New(context.Background())
	if _, err := m.TrustHostKey(context.Background(), "ssh://desktop", "desktop ssh-ed25519 AAAA"); !errors.Is(err, ErrKeyNotOffered) {
		t.Fatalf("want ErrKeyNotOffered, got %v", err)
	}
	tgt, _ := ParseURL("ssh://desktop")
	m.recordOffer(tgt, &UnknownHostKeyError{Target: tgt, Name: "desktop", KnownHostsPath: filepath.Join(t.TempDir(), "kh"),
		Keys: []HostKey{{Type: "ssh-ed25519", Fingerprint: "SHA256:x", Line: "desktop ssh-ed25519 AAAA"}}})
	if _, err := m.TrustHostKey(context.Background(), "ssh://desktop", "desktop ssh-ed25519 FORGED"); !errors.Is(err, ErrKeyNotOffered) {
		t.Fatalf("a line that was not offered must be refused: %v", err)
	}
	if len(m.offered) != 1 {
		t.Fatal("a refused line must not consume the offer")
	}
}
