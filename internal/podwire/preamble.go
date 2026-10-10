package podwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// A preamble is a stream's first bytes: the tag, a 4-byte big-endian header
// length (1 to MaxHeader) and the header, one JSON object. Its encoding never
// changes between protocol versions.
const preambleSize = 5

// ErrHeaderTooLarge is a preamble whose header is over MaxHeader.
var ErrHeaderTooLarge = fmt.Errorf("stream header is over %d bytes", MaxHeader)

// WritePreamble writes a stream's preamble in one Write: tag, then header as
// JSON.
func WritePreamble(w io.Writer, tag Tag, header any) error {
	body, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("encode %s header: %w", tag, err)
	}
	if len(body) > MaxHeader {
		return ErrHeaderTooLarge
	}
	buf := make([]byte, preambleSize+len(body))
	buf[0] = byte(tag)
	binary.BigEndian.PutUint32(buf[1:preambleSize], uint32(len(body)))
	copy(buf[preambleSize:], body)
	_, err = w.Write(buf)
	return err
}

// ReadPreamble reads a stream's preamble and returns its tag and raw header,
// which is a JSON object (DecodeHeader parses it). It reads exactly the
// preamble, so r is left at the stream's first frame.
func ReadPreamble(r io.Reader) (Tag, []byte, error) {
	var head [preambleSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}
		return 0, nil, fmt.Errorf("read stream preamble: %w", err)
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n == 0 {
		return 0, nil, errors.New("stream header is empty")
	}
	if n > MaxHeader {
		return 0, nil, ErrHeaderTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, fmt.Errorf("read stream header: %w", noEOF(err))
	}
	if !isJSONObject(body) {
		return 0, nil, errors.New("stream header is not a JSON object")
	}
	return Tag(head[0]), body, nil
}

// DecodeHeader parses a raw header into v. Unknown fields are ignored, so an
// older peer takes a newer one's header.
func DecodeHeader(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse stream header: %w", err)
	}
	return nil
}

// isJSONObject reports whether b is one valid JSON object.
func isJSONObject(b []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(b, &obj) == nil && obj != nil
}

// noEOF turns a clean EOF inside a preamble or frame into the truncation it is.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
