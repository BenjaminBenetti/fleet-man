package control

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// writeTimeout bounds a single Send's write to the control socket. It is
// defense-in-depth: a host that has stopped reading (e.g. its TUI is hung)
// lets the kernel send buffer fill, after which an unbounded write would block
// the caller — potentially the in-instance UI thread — indefinitely. With a
// deadline a stuck host instead surfaces as a timeout error the caller can
// report, rather than a permanent freeze. It is generous enough that a merely
// busy host won't trip it.
const writeTimeout = 5 * time.Second

// Client is the instance-side end of the control channel. It holds a single
// connection to the host's listening socket and writes newline-delimited
// Envelopes. One Client is intended to live for the lifetime of an
// in-container process (e.g. the `fleet launch` TUI); each Send writes one
// JSON line the host's Server decodes and dispatches.
type Client struct {
	conn net.Conn
	// mu serialises writes so concurrent Send calls can't interleave bytes on
	// the wire and corrupt the newline-delimited framing.
	mu  sync.Mutex
	enc *json.Encoder
}

// Dial connects to the control socket at socketPath (normally
// ContainerSocketPath inside an instance). A failure means the host isn't
// listening — there is no socket, or no Server has been started for this
// instance — and the caller should treat it as "host not available" and run
// in a degraded mode (e.g. still render its UI but disable host-driven
// actions) rather than aborting.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial control socket %s: %w", socketPath, err)
	}
	// json.Encoder writes each value followed by a newline, which is exactly
	// the newline-delimited framing the Server reads with a json.Decoder.
	return &Client{conn: conn, enc: json.NewEncoder(conn)}, nil
}

// Send marshals payload, wraps it in an Envelope{Type: messageType}, and writes
// it as one JSON line. payload may be nil for a type-only message. Safe for
// concurrent use: a mutex serialises the underlying write so two goroutines
// can't interleave bytes mid-line.
func (c *Client) Send(messageType string, payload any) error {
	var rawPayload json.RawMessage
	if payload != nil {
		marshaled, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal control payload: %w", err)
		}
		rawPayload = marshaled
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Bound the write so a host that has stopped reading can't block the caller
	// indefinitely once the kernel send buffer fills. A best-effort deadline:
	// connections that don't support one simply ignore the error here.
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := c.enc.Encode(Envelope{Type: messageType, Payload: rawPayload}); err != nil {
		return fmt.Errorf("send control envelope: %w", err)
	}
	return nil
}

// OpenBrowser is a typed convenience over Send for TypeOpenBrowser: it asks
// the host to open (or navigate) its browser to url.
func (c *Client) OpenBrowser(url string) error {
	return c.Send(TypeOpenBrowser, OpenBrowserPayload{URL: url})
}

// CopyFile is a typed convenience over Send for TypeCopyFile: it asks the host
// to copy the file at the absolute in-instance path out to the user's machine —
// to dest there when given, the user's downloads folder otherwise.
func (c *Client) CopyFile(path, dest string) error {
	return c.Send(TypeCopyFile, CopyFilePayload{Path: path, Dest: dest})
}

// Close closes the underlying connection. It is safe to call once; further
// Sends will fail.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}
