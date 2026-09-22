// Package agentproto is the ssh-agent protocol plumbing both halves of
// SSH-agent forwarding share: reassembling length-prefixed messages, the
// request allowlist, and the serial serve loop that answers a relayed
// connection one request at a time. It is pure (no fleet imports), so the
// client provider (internal/agentfwd) and the daemon (internal/server) use
// the same code.
package agentproto

import (
	"encoding/binary"
	"errors"
	"io"
)

// A relayed connection reaches the agent as a LOCAL client: no ssh client
// bound it to a forwarding hop. OpenSSH's agent grants a local client more
// than a forwarded one: it will load PKCS#11 and security-key provider
// libraries for it (refused for forwarded clients since OpenSSH 9.3p2,
// CVE-2023-38408). So only what `ssh -A` exists for is let through — listing
// keys and signing, plus the session-bind a remote ssh client sends so
// destination constraints on its own hop still apply — and everything else
// (add or remove keys, lock/unlock, smartcard and provider loading) is
// answered with SSH_AGENT_FAILURE without the agent ever seeing it.

// Agent protocol message types (draft-miller-ssh-agent).
const (
	agentFailure            = 5
	agentcRequestIdentities = 11
	agentcSignRequest       = 13
	agentcExtension         = 27
)

// maxAgentMessage is OpenSSH's AGENT_MAX_LEN: nothing legitimate is bigger.
const maxAgentMessage = 256 * 1024

// allowedExtensions are the SSH_AGENTC_EXTENSION names passed through.
var allowedExtensions = map[string]bool{
	"session-bind@openssh.com": true, // a remote ssh client binding its own hop
	"query":                    true, // which extensions the agent supports
}

// FailureReply is a complete SSH_AGENT_FAILURE message.
var FailureReply = []byte{0, 0, 0, 1, agentFailure}

// ErrMessageTooLarge is returned for a message over the protocol limit.
var ErrMessageTooLarge = errors.New("agent message exceeds the protocol limit")

// RequestAllowed reports whether a complete request message (length prefix
// included) may reach the agent.
func RequestAllowed(msg []byte) bool {
	if len(msg) < 5 {
		return false
	}
	switch msg[4] {
	case agentcRequestIdentities, agentcSignRequest:
		return true
	case agentcExtension:
		body := msg[5:]
		if len(body) < 4 {
			return false
		}
		n := binary.BigEndian.Uint32(body)
		if uint64(n) > uint64(len(body)-4) {
			return false
		}
		return allowedExtensions[string(body[4:4+n])]
	default:
		return false
	}
}

// MessageReader reassembles length-prefixed agent messages from a byte stream
// that arrives in arbitrary chunks (one request may span data frames, and one
// frame may carry several requests).
type MessageReader struct {
	buf []byte
	// Next returns the next chunk of the stream; ok=false at its end.
	Next func() (chunk []byte, ok bool)
}

// Read returns the next complete message (length prefix included), io.EOF at
// a clean end of stream, or an error for a truncated or oversized message.
func (r *MessageReader) Read() ([]byte, error) {
	for {
		if len(r.buf) >= 4 {
			n := binary.BigEndian.Uint32(r.buf)
			if n > maxAgentMessage {
				return nil, ErrMessageTooLarge
			}
			if total := 4 + int(n); len(r.buf) >= total {
				msg := append([]byte(nil), r.buf[:total]...)
				r.buf = r.buf[total:]
				return msg, nil
			}
		}
		chunk, ok := r.Next()
		if !ok {
			if len(r.buf) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		r.buf = append(r.buf, chunk...)
	}
}

// ReadMessage reads one complete message from r.
func ReadMessage(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > maxAgentMessage {
		return nil, ErrMessageTooLarge
	}
	msg := make([]byte, 4+int(n))
	copy(msg, header[:])
	if _, err := io.ReadFull(r, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}

// ChunkReader adapts an io.Reader (a relayed connection) to MessageReader.Next.
func ChunkReader(r io.Reader) func() ([]byte, bool) {
	buf := make([]byte, 32*1024)
	return func() ([]byte, bool) {
		n, err := r.Read(buf)
		if n > 0 {
			return append([]byte(nil), buf[:n]...), true
		}
		if err != nil {
			return nil, false
		}
		return nil, true
	}
}

// Serve answers requests one at a time: each is either forwarded to agent
// (filter off, or allowed by RequestAllowed) and its reply read back, or
// answered with FailureReply here. reply delivers each answer and returns
// false to stop. Serving strictly in turn keeps replies in request order even
// when some are answered here, and a client that half-closes after its last
// request still gets every reply (the agent never sees the half-close —
// OpenSSH's agent drops a connection on EOF without answering what is
// pending). Returns nil at the requests' clean end.
func Serve(requests *MessageReader, agent io.ReadWriter, filter bool, reply func([]byte) bool) error {
	for {
		request, err := requests.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		answer := FailureReply
		if !filter || RequestAllowed(request) {
			if _, err := agent.Write(request); err != nil {
				return err
			}
			if answer, err = ReadMessage(agent); err != nil {
				return err
			}
		}
		if !reply(answer) {
			return nil
		}
	}
}
