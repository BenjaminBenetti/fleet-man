package agentproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

// agentMessage frames body as an agent-protocol message.
func agentMessage(body ...byte) []byte {
	msg := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(msg, uint32(len(body)))
	return append(msg, body...)
}

// extensionRequest is SSH_AGENTC_EXTENSION with the given name.
func extensionRequest(name string) []byte {
	body := []byte{agentcExtension, 0, 0, 0, byte(len(name))}
	return agentMessage(append(body, name...)...)
}

func TestRequestAllowed(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		want bool
	}{
		{"list identities", agentMessage(agentcRequestIdentities), true},
		{"sign", agentMessage(agentcSignRequest, 0, 0, 0, 0), true},
		{"session-bind", extensionRequest("session-bind@openssh.com"), true},
		{"query", extensionRequest("query"), true},
		{"other extension", extensionRequest("restrict-destination-v00@openssh.com"), false},
		{"add identity", agentMessage(17), false},
		{"remove identity", agentMessage(18), false},
		{"remove all", agentMessage(19), false},
		{"add smartcard (PKCS#11 provider)", agentMessage(20), false},
		{"remove smartcard", agentMessage(21), false},
		{"lock", agentMessage(22), false},
		{"unlock", agentMessage(23), false},
		{"add constrained identity (SK provider)", agentMessage(25), false},
		{"add constrained smartcard", agentMessage(26), false},
		{"SSH1 list", agentMessage(1), false},
		{"empty", agentMessage(), false},
		{"extension with a lying name length", agentMessage(agentcExtension, 0, 0, 0, 99, 'q'), false},
	}
	for _, c := range cases {
		if got := RequestAllowed(c.msg); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMessageReaderReassemblesAcrossAndWithinChunks(t *testing.T) {
	a := agentMessage(agentcRequestIdentities)
	b := agentMessage(agentcSignRequest, 1, 2, 3)
	stream := append(append([]byte(nil), a...), b...)
	// Split mid-length-prefix, mid-body, and with two messages' boundary
	// inside one chunk.
	chunks := [][]byte{stream[:2], stream[2:6], stream[6:]}
	r := &MessageReader{Next: func() ([]byte, bool) {
		if len(chunks) == 0 {
			return nil, false
		}
		c := chunks[0]
		chunks = chunks[1:]
		return c, true
	}}
	for i, want := range [][]byte{a, b} {
		got, err := r.Read()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("message %d = %v, %v; want %v", i, got, err, want)
		}
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}
}

func TestMessageReaderRejectsTruncatedAndOversized(t *testing.T) {
	once := func(chunk []byte) func() ([]byte, bool) {
		done := false
		return func() ([]byte, bool) {
			if done {
				return nil, false
			}
			done = true
			return chunk, true
		}
	}
	if _, err := (&MessageReader{Next: once([]byte{0, 0, 0, 5, 11})}).Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
	huge := make([]byte, 4)
	binary.BigEndian.PutUint32(huge, maxAgentMessage+1)
	if _, err := (&MessageReader{Next: once(huge)}).Read(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
}

func TestBindAsForwardedFailsClosedWithoutAKey(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := BindAsForwarded(a, nil); err == nil {
		t.Fatal("a connection that cannot be bound as forwarded must be refused, not used unbound")
	}
}
