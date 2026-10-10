package execas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A request frame is one header line, "exec-as/1 <length>\n", then exactly
// <length> bytes of the Request as JSON. Nothing is read past it: whatever
// follows on the stream is the command's own stdin. The header is text so a
// shell can write a frame with printf.
const (
	frameVersion = "exec-as/1"
	// MaxRequestSize caps a frame's JSON. Scripts travel in requests, and
	// Linux takes 2 MiB of argv plus environment at most.
	MaxRequestSize = 1 << 20
	// maxFrameHeader is the longest header line: version, a space, the
	// length's digits and the newline.
	maxFrameHeader = len(frameVersion) + 1 + 7 + 1
)

// WriteRequest writes req to w as one frame.
func WriteRequest(w io.Writer, req Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(body) > MaxRequestSize {
		return fmt.Errorf("exec request is %d bytes, more than the %d a frame holds", len(body), MaxRequestSize)
	}
	frame := make([]byte, 0, maxFrameHeader+len(body))
	frame = append(frame, frameVersion+" "+strconv.Itoa(len(body))+"\n"...)
	frame = append(frame, body...)
	_, err = w.Write(frame)
	return err
}

// ReadRequest reads one frame from r and returns its request, checked. It
// reads the header a byte at a time and the body exactly, so r is left at the
// first byte after the frame: pass it an unbuffered reader whose remainder
// belongs to the command.
func ReadRequest(r io.Reader) (Request, error) {
	header, err := readFrameHeader(r)
	if err != nil {
		return Request{}, err
	}
	version, size, ok := strings.Cut(header, " ")
	if !ok || !strings.HasPrefix(version, "exec-as/") {
		return Request{}, fmt.Errorf("not an exec request frame: %q", header)
	}
	if version != frameVersion {
		return Request{}, fmt.Errorf("unsupported exec request frame %s (this fleet speaks %s)", version, frameVersion)
	}
	n, err := strconv.Atoi(size)
	if err != nil || n <= 0 || size != strconv.Itoa(n) {
		return Request{}, fmt.Errorf("bad exec request length %q", size)
	}
	if n > MaxRequestSize {
		return Request{}, fmt.Errorf("exec request is %d bytes, more than the %d a frame holds", n, MaxRequestSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Request{}, fmt.Errorf("read exec request: %w", noEOF(err))
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, fmt.Errorf("parse exec request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

// readFrameHeader reads up to and excluding the header's newline.
func readFrameHeader(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for len(line) < maxFrameHeader {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return string(line), nil
			}
			line = append(line, b[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return "", errors.New("no exec request on stdin")
			}
			return "", fmt.Errorf("read exec request header: %w", noEOF(err))
		}
	}
	return "", fmt.Errorf("not an exec request frame: header longer than %d bytes", maxFrameHeader)
}

// noEOF turns a clean EOF inside a frame into the truncation it is.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
