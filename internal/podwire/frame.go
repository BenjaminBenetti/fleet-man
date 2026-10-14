package podwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// FrameType is a frame's kind: its first byte.
type FrameType byte

// Frame types. JSON payloads are objects whose unknown fields a receiver
// ignores; a receiver skips a frame type it does not know.
const (
	// FrameData is stdout (stdin, from the client), file data, an RPC
	// result's bytes, or a control message.
	FrameData FrameType = 0x01
	// FrameStderr is a command's stderr.
	FrameStderr FrameType = 0x02
	// FrameEOF half-closes: the end of a command's stdin, or of file data.
	FrameEOF FrameType = 0x03
	// FrameResize sets a TTY's size: Resize.
	FrameResize FrameType = 0x04
	// FrameSignal sends a signal to a command's foreground: Signal.
	FrameSignal FrameType = 0x05
	// FrameExit ends a stream with its status: ExitStatus for an exec,
	// Result for a file or an RPC. On a resumable exec the client answers the
	// agent's EXIT with an empty EXIT, so the agent stops holding it.
	FrameExit FrameType = 0x06
	// FrameOpen is the accepting side's first answer to a preamble:
	// OpenReply, or FileInfo for a file read.
	FrameOpen FrameType = 0x07
	// FramePing is a heartbeat; on a resumable exec it carries the sender's
	// acknowledgements (Ping).
	FramePing FrameType = 0x08
	// FrameResume answers a re-attach: ResumeReply.
	FrameResume FrameType = 0x09
	// FrameAbort ends a command now (its process group). It is the framed
	// form of a FIN before EXIT, for transports with no FIN of their own (a
	// direct exec's stdio, where the end of stdin is also what a dropped
	// connection looks like).
	FrameAbort FrameType = 0x0a
)

var frameNames = map[FrameType]string{
	FrameData:   "DATA",
	FrameStderr: "STDERR",
	FrameEOF:    "EOF",
	FrameResize: "RESIZE",
	FrameSignal: "SIGNAL",
	FrameExit:   "EXIT",
	FrameOpen:   "OPEN",
	FramePing:   "PING",
	FrameResume: "RESUME",
	FrameAbort:  "ABORT",
}

func (t FrameType) String() string {
	if name, ok := frameNames[t]; ok {
		return name
	}
	return fmt.Sprintf("FRAME-0x%02x", byte(t))
}

// frameHeader is a frame's type byte and 4-byte big-endian payload length.
const frameHeader = 5

// Frame is one frame.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// ErrFrameTooLarge is a frame whose payload is over MaxFrame.
var ErrFrameTooLarge = fmt.Errorf("frame is over %d bytes", MaxFrame)

// FrameReader reads frames from a stream.
type FrameReader struct {
	r    io.Reader
	head [frameHeader]byte
}

// NewFrameReader reads frames from r.
func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// Next reads the next frame. A clean end of stream between frames is io.EOF;
// one inside a frame is io.ErrUnexpectedEOF. The payload is the caller's.
func (fr *FrameReader) Next() (Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.head[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(fr.head[1:])
	if n > MaxFrame {
		return Frame{}, ErrFrameTooLarge
	}
	f := Frame{Type: FrameType(fr.head[0])}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(fr.r, f.Payload); err != nil {
			return Frame{}, noEOF(err)
		}
	}
	return f, nil
}

// FrameWriter writes frames to a stream. It is safe for concurrent use: each
// frame is one Write, never interleaved with another.
type FrameWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

// NewFrameWriter writes frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write writes one frame.
func (fw *FrameWriter) Write(t FrameType, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.buf = append(fw.buf[:0], byte(t), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(fw.buf[1:frameHeader], uint32(len(payload)))
	fw.buf = append(fw.buf, payload...)
	_, err := fw.w.Write(fw.buf)
	if cap(fw.buf) > 2*DataChunk+frameHeader {
		fw.buf = nil
	}
	return err
}

// WriteData writes p as DataChunk-sized frames of type t.
func (fw *FrameWriter) WriteData(t FrameType, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), DataChunk)
		if err := fw.Write(t, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// WriteJSON writes v as one frame of type t.
func (fw *FrameWriter) WriteJSON(t FrameType, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", t, err)
	}
	return fw.Write(t, body)
}

// DecodeFrame parses a JSON frame payload into v. An empty payload leaves v
// as it is.
func DecodeFrame(f Frame, v any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("parse %s frame: %w", f.Type, err)
	}
	return nil
}
