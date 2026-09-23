package agentproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// bind.go marks a relayed connection as FORWARDED to the agent, the way
// `ssh -A` does: before any request, the connection is bound with
// session-bind@openssh.com and is_forwarding=1. OpenSSH's agent then applies
// its rules for remote clients (no PKCS#11 / security-key provider loading,
// on top of the allowlist) and its destination constraints: a key added with
// `ssh-add -h host` for use from this machine only is neither listed nor
// usable through the relay. The "host key" is a throwaway one — the agent can
// only check that it signed the session id, which is all a relay can offer —
// so no constrained key ever matches it.

const bindTimeout = 5 * time.Second

// NewBindKey makes a throwaway key to sign forwarding binds with.
func NewBindKey() ssh.Signer {
	// Neither step can fail: crypto/rand never returns an error (Go 1.24+),
	// and any ed25519 key makes a signer.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("agentproto: ed25519 key generation failed: " + err.Error())
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		panic("agentproto: ed25519 signer: " + err.Error())
	}
	return signer
}

// BindAsForwarded sends the forwarding bind on agent and reads the answer.
// Any answer counts — an agent without the extension (or an older OpenSSH)
// answers with a failure, which is fine: the allowlist still applies. An
// error means the connection is unusable: the answer did not arrive in time
// and could still turn up later as the "reply" to the next request, shifting
// every reply after it. The caller must then drop the connection.
func BindAsForwarded(agent net.Conn, key ssh.Signer) error {
	// Fail closed: every relayed connection is bound, or it is not used —
	// an unbound one reaches the agent as a local client.
	if key == nil {
		return errors.New("no key to sign the forwarding bind with")
	}
	msg, err := ForwardingBind(key)
	if err != nil {
		return err
	}
	if err := agent.SetDeadline(time.Now().Add(bindTimeout)); err != nil {
		return err
	}
	defer agent.SetDeadline(time.Time{})
	if _, err := agent.Write(msg); err != nil {
		return err
	}
	_, err = ReadMessage(agent)
	return err
}

// ForwardingBind builds a complete session-bind@openssh.com request binding a
// fresh session id, signed by key, with is_forwarding set.
func ForwardingBind(key ssh.Signer) ([]byte, error) {
	sessionID := make([]byte, 32)
	if _, err := rand.Read(sessionID); err != nil {
		return nil, err
	}
	sig, err := key.Sign(rand.Reader, sessionID)
	if err != nil {
		return nil, err
	}
	body := []byte{agentcExtension}
	body = appendString(body, []byte("session-bind@openssh.com"))
	body = appendString(body, key.PublicKey().Marshal())
	body = appendString(body, sessionID)
	body = appendString(body, ssh.Marshal(sig))
	body = append(body, 1) // is_forwarding
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return append(msg, body...), nil
}

func appendString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}
