package kubeshim

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The exec socket carries one exec per connection as frames: a type byte, a
// big-endian uint32 payload length, the payload. The shim sends the request
// first, then its stdin and resizes; the server sends the command's output
// and finally its exit code (or why it could not run it). A connection that
// closes before the exit frame is an exec abandoned by the shim (the server
// ends the command) or lost by the server (the shim fails).
const (
	frameRequest  byte = 1 // shim → server: ExecRequest JSON
	frameStdin    byte = 2 // shim → server: stdin bytes
	frameStdinEOF byte = 3 // shim → server: stdin closed
	frameResize   byte = 4 // shim → server: WindowSize JSON
	frameStdout   byte = 5 // server → shim: stdout bytes
	frameStderr   byte = 6 // server → shim: stderr bytes
	frameExit     byte = 7 // server → shim: exit code, big-endian int32
	frameError    byte = 8 // server → shim: why the exec could not run
)

const (
	// maxFrame bounds a frame's payload. The request (argv and environment)
	// is the largest; data frames are chunkSize at most.
	maxFrame = 4 << 20
	// chunkSize is the largest data frame a writer sends.
	chunkSize = 32 << 10
)

// errFrameTooLarge is a frame over maxFrame.
var errFrameTooLarge = errors.New("exec socket frame too large")

// readFrame reads one frame from r.
func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// frameWriter serialises frames onto one connection from several goroutines.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// write sends one frame.
func (fw *frameWriter) write(typ byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := fw.w.Write(payload)
	return err
}

// exitPayload encodes an exit code.
func exitPayload(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}

// exitCode decodes an exit frame.
func exitCode(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("exit frame of %d bytes", len(payload))
	}
	return int(int32(binary.BigEndian.Uint32(payload))), nil
}

// streamWriter is an io.Writer that sends what it is given as data frames of
// one type.
type streamWriter struct {
	fw  *frameWriter
	typ byte
}

func (sw streamWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > chunkSize {
			n = chunkSize
		}
		if err := sw.fw.write(sw.typ, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
