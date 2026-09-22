package agentfwd

import (
	"encoding/binary"
	"errors"
	"io"
)

// filter.go limits what a relayed connection may ask of the user's agent.
//
// A connection the provider dials reaches the agent as a LOCAL client: no
// ssh client bound it to a forwarding hop (session-bind@openssh.com with
// is_forwarding=1, which needs the remote host key and the session id — a
// relay cannot produce one). OpenSSH's agent grants a local client more than a
// forwarded one: it will load PKCS#11 and security-key provider libraries for
// it (refused for forwarded clients since OpenSSH 9.3p2, CVE-2023-38408). So
// the provider itself only lets through what `ssh -A` exists for — listing
// keys and signing, plus the session-bind a remote ssh client sends so
// destination constraints on its own hop still apply — and answers everything
// else (add or remove keys, lock/unlock, smartcard and provider loading) with
// SSH_AGENT_FAILURE, without the agent ever seeing it.

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

// failureReply is a complete SSH_AGENT_FAILURE message.
var failureReply = []byte{0, 0, 0, 1, agentFailure}

var errAgentMessageTooLarge = errors.New("agent message exceeds the protocol limit")

// requestAllowed reports whether a complete request message (length prefix
// included) may reach the agent.
func requestAllowed(msg []byte) bool {
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

// messageReader reassembles length-prefixed agent messages from a byte stream
// that arrives in arbitrary chunks (one request may span data frames, and one
// frame may carry several requests).
type messageReader struct {
	buf []byte
	// next returns the next chunk of the stream; ok=false at its end.
	next func() (chunk []byte, ok bool)
}

// read returns the next complete message (length prefix included), io.EOF at
// a clean end of stream, or an error for a truncated or oversized message.
func (r *messageReader) read() ([]byte, error) {
	for {
		if len(r.buf) >= 4 {
			n := binary.BigEndian.Uint32(r.buf)
			if n > maxAgentMessage {
				return nil, errAgentMessageTooLarge
			}
			if total := 4 + int(n); len(r.buf) >= total {
				msg := append([]byte(nil), r.buf[:total]...)
				r.buf = r.buf[total:]
				return msg, nil
			}
		}
		chunk, ok := r.next()
		if !ok {
			if len(r.buf) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		r.buf = append(r.buf, chunk...)
	}
}

// readAgentMessage reads one complete reply from the agent.
func readAgentMessage(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > maxAgentMessage {
		return nil, errAgentMessageTooLarge
	}
	msg := make([]byte, 4+int(n))
	copy(msg, header[:])
	if _, err := io.ReadFull(r, msg[4:]); err != nil {
		return nil, err
	}
	return msg, nil
}
