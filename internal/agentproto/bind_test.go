package agentproto

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// realAgent starts OpenSSH's ssh-agent with two keys: one unconstrained, one
// restricted with `ssh-add -h` to direct use against some host. Skips when
// OpenSSH (8.9+, for destination constraints) is not installed.
func realAgent(t *testing.T) (sock string, free, constrained ssh.PublicKey) {
	t.Helper()
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "afb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock = filepath.Join(dir, "agent.sock")
	agentCmd := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agentCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = agentCmd.Process.Kill()
		_ = agentCmd.Wait()
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		exec.Command("sleep", "0.02").Run()
	}
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "illegal option") || strings.Contains(string(out), "unknown option") {
				t.Skipf("ssh-add has no destination constraints: %s", out)
			}
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	key := func(name string) ssh.PublicKey {
		t.Helper()
		path := filepath.Join(dir, name)
		run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path)
		data, err := os.ReadFile(path + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			t.Fatal(err)
		}
		return pub
	}
	free = key("free")
	constrained = key("constrained")
	// ssh-add -h needs the destination's host key, from a known_hosts file.
	hostKey := key("prodhost")
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("prod.example.com "+string(ssh.MarshalAuthorizedKey(hostKey))), 0o600); err != nil {
		t.Fatal(err)
	}
	run("ssh-add", "-q", filepath.Join(dir, "free"))
	run("ssh-add", "-q", "-H", knownHosts, "-h", "prod.example.com", filepath.Join(dir, "constrained"))
	return sock, free, constrained
}

func listKeys(t *testing.T, conn net.Conn) []*agent.Key {
	t.Helper()
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return keys
}

func hasKey(keys []*agent.Key, want ssh.PublicKey) bool {
	for _, k := range keys {
		if string(k.Marshal()) == string(want.Marshal()) {
			return true
		}
	}
	return false
}

func TestForwardingBindAppliesTheAgentsDestinationConstraints(t *testing.T) {
	sock, free, constrained := realAgent(t)

	// Unbound (what a plain relay would be): the agent treats the connection
	// as local and offers the key constrained to direct use.
	plain, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if keys := listKeys(t, plain); !hasKey(keys, constrained) {
		t.Fatalf("control: an unbound connection should see the constrained key, got %v", keys)
	}

	// Bound as forwarded, like every relayed connection: the constrained key
	// is gone, the unconstrained one still signs.
	bound, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	key, err := NewBindKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := BindAsForwarded(bound, key); err != nil {
		t.Fatalf("bind: %v", err)
	}
	keys := listKeys(t, bound)
	if hasKey(keys, constrained) {
		t.Fatal("a destination-constrained key must not be offered through the relay")
	}
	if !hasKey(keys, free) {
		t.Fatalf("the unconstrained key must still be offered, got %v", keys)
	}
	sig, err := agent.NewClient(bound).Sign(free, []byte("data"))
	if err != nil {
		t.Fatalf("sign with the unconstrained key: %v", err)
	}
	if err := free.Verify([]byte("data"), sig); err != nil {
		t.Fatal(err)
	}
}

func TestForwardingBindIsAWellFormedExtension(t *testing.T) {
	sock, _, _ := realAgent(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	signer, err := NewBindKey()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ForwardingBind(signer)
	if err != nil {
		t.Fatal(err)
	}
	if !RequestAllowed(msg) {
		t.Fatal("the bind itself must pass the allowlist (a remote client's own bind is the same extension)")
	}
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	reply, err := ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if reply[4] != 6 { // SSH_AGENT_SUCCESS
		t.Fatalf("OpenSSH's agent rejected the forwarding bind: reply type %d", reply[4])
	}
}

// authBind builds the session-bind@openssh.com an ssh client inside an
// instance sends for its own hop: a fresh session id signed by hostKey, with
// is_forwarding=0 (the connection authenticates to that host). Built here,
// not from ForwardingBind, so the test cannot inherit a bug in it.
func authBind(t *testing.T, hostKey ssh.Signer) []byte {
	t.Helper()
	sessionID := make([]byte, 32)
	if _, err := rand.Read(sessionID); err != nil {
		t.Fatal(err)
	}
	sig, err := hostKey.Sign(rand.Reader, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte{27} // SSH_AGENTC_EXTENSION
	body = appendString(body, []byte("session-bind@openssh.com"))
	body = appendString(body, hostKey.PublicKey().Marshal())
	body = appendString(body, sessionID)
	body = appendString(body, ssh.Marshal(sig))
	body = append(body, 0) // is_forwarding
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

// TestForwardingBindLeavesTheHopToTheInstance: the relay's bind must say
// is_forwarding=1. OpenSSH's agent refuses any further bind on a connection
// already bound for authentication (is_forwarding=0), so with the flag wrong
// the bind an ssh client inside an instance sends for its own hop would fail.
// Bound as forwarded, that hop binds and the unconstrained key still signs.
func TestForwardingBindLeavesTheHopToTheInstance(t *testing.T) {
	sock, free, _ := realAgent(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	relayKey, err := NewBindKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := BindAsForwarded(conn, relayKey); err != nil {
		t.Fatalf("bind: %v", err)
	}

	hopKey, err := NewBindKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(authBind(t, hopKey)); err != nil {
		t.Fatal(err)
	}
	reply, err := ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if reply[4] != 6 { // SSH_AGENT_SUCCESS
		t.Fatalf("the agent refused the instance's own hop bind after the relay's (reply type %d): the relay's bind must set is_forwarding", reply[4])
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	sig, err := agent.NewClient(conn).Sign(free, []byte("data"))
	if err != nil {
		t.Fatalf("sign with the unconstrained key after the hop bind: %v", err)
	}
	if err := free.Verify([]byte("data"), sig); err != nil {
		t.Fatal(err)
	}
}
