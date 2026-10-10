package podwire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
)

// FileTrailer is the EOF frame's payload that ends a file write: what the
// client sent, which the agent checks before it renames the file into place.
type FileTrailer struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// WriteFile writes data to a file in the pod, atomically, as h's user: a temp
// file beside h.Path, checked against the size and sha256 actually sent (and
// h's, when it names them), then renamed into place. h.Size is the size when
// known (-1 when not); the agent session refuses more than SmallFile with
// CodeTooLarge (decide by size first: data is consumed), the bulk session
// takes any size. ctx ending abandons the write, leaving nothing behind.
func (c *Client) WriteFile(ctx context.Context, h FileWriteHeader, data io.Reader) error {
	t, err := c.open(ctx, TagFileWrite, h)
	if err != nil {
		return err
	}
	defer t.Close()
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()

	// The agent may answer early (a refusal, or an error as it writes); the
	// answer is read while the data goes out.
	results := make(chan error, 1)
	go func() { results <- readResult(NewFrameReader(t), nil) }()

	w := NewFrameWriter(t)
	sum := sha256.New()
	size, sendErr := copyFrames(w, io.TeeReader(data, sum), results)
	if sendErr == nil {
		sendErr = w.WriteJSON(FrameEOF, FileTrailer{Size: size, SHA256: hex.EncodeToString(sum.Sum(nil))})
	}
	select {
	case err := <-results:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && sendErr != nil {
			return sendErr
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// copyFrames sends r as DATA frames until it ends or the agent answers.
func copyFrames(w *FrameWriter, r io.Reader, answered <-chan error) (int64, error) {
	buf := make([]byte, DataChunk)
	var total int64
	for {
		if len(answered) > 0 {
			return total, errors.New("the agent ended the write early")
		}
		n, err := r.Read(buf)
		if n > 0 {
			if werr := w.Write(FrameData, buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, fmt.Errorf("read the file to write: %w", err)
		}
	}
}

// readResult reads a file or RPC stream's answer up to its EXIT and returns
// the Result's error. DATA frames go to data (nil: refused as unexpected),
// capped at MaxReply. An OPEN refusal is an error.
func readResult(r *FrameReader, data *bytes.Buffer) error {
	for {
		f, err := r.Next()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrConnectionLost, noEOF(err))
		}
		switch f.Type {
		case FrameOpen:
			var ref Refusal
			if DecodeFrame(f, &ref) == nil && (ref.Refused != "" || ref.Error != "") {
				return fmt.Errorf("%w: %s", ErrRefused, ref.Error)
			}
		case FrameData:
			if data == nil {
				continue
			}
			if data.Len()+len(f.Payload) > MaxReply {
				return ErrReplyTooLarge
			}
			data.Write(f.Payload)
		case FrameExit:
			var res Result
			if err := DecodeFrame(f, &res); err != nil {
				return err
			}
			return remoteError(res.Code, res.Error)
		}
	}
}

// ReadFile reads a file in the pod as h's user, buffered: at most h.Max (when
// set) and MaxReply. The agent session refuses a file over SmallFile with
// CodeTooLarge; read it over the bulk session (OpenFile streams any size).
func (c *Client) ReadFile(ctx context.Context, h FileReadHeader) ([]byte, error) {
	if h.Max <= 0 || h.Max > MaxReply {
		h.Max = MaxReply
	}
	f, err := c.OpenFile(ctx, h)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	data, err := io.ReadAll(f)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return data, nil
}

// FileReader is a file streaming out of the pod: Read returns its data, then
// io.EOF once the agent's sha256 has matched what arrived.
type FileReader struct {
	Info FileInfo

	t   io.ReadWriteCloser
	r   *FrameReader
	sum hash.Hash
	n   int64
	max int64
	buf []byte
	err error
}

// OpenFile starts reading a file in the pod as h's user and returns once the
// agent has opened it. Close the reader when done.
func (c *Client) OpenFile(ctx context.Context, h FileReadHeader) (*FileReader, error) {
	t, err := c.open(ctx, TagFileRead, h)
	if err != nil {
		return nil, err
	}
	r := NewFrameReader(t)
	f, err := firstFrame(ctx, t, r)
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("read %s: %w", h.Path, err)
	}
	var info FileInfo
	if f.Type != FrameOpen || DecodeFrame(f, &info) != nil {
		_ = t.Close()
		return nil, fmt.Errorf("read %s: unexpected %s frame", h.Path, f.Type)
	}
	if info.Refused != "" {
		_ = t.Close()
		return nil, fmt.Errorf("%w: %s", ErrRefused, info.Error)
	}
	if err := remoteError(info.Code, info.Error); err != nil {
		_ = t.Close()
		return nil, err
	}
	if h.Max > 0 && info.Size > h.Max {
		_ = t.Close()
		return nil, &RemoteError{Code: CodeTooLarge, Message: fmt.Sprintf("%s is %d bytes, over %d", h.Path, info.Size, h.Max)}
	}
	return &FileReader{Info: info, t: t, r: r, sum: sha256.New(), max: h.Max}, nil
}

func (f *FileReader) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		fr, err := f.r.Next()
		if err != nil {
			f.err = fmt.Errorf("%w: %v", ErrConnectionLost, noEOF(err))
			continue
		}
		switch fr.Type {
		case FrameData:
			f.n += int64(len(fr.Payload))
			if f.max > 0 && f.n > f.max {
				f.err = &RemoteError{Code: CodeTooLarge, Message: fmt.Sprintf("file grew past %d bytes", f.max)}
				continue
			}
			f.sum.Write(fr.Payload)
			f.buf = fr.Payload
		case FrameExit:
			var res Result
			if err := DecodeFrame(fr, &res); err != nil {
				f.err = err
			} else if err := remoteError(res.Code, res.Error); err != nil {
				f.err = err
			} else if got := hex.EncodeToString(f.sum.Sum(nil)); res.SHA256 != got || res.Size != f.n {
				f.err = &RemoteError{Code: CodeMismatch, Message: fmt.Sprintf("file read: got %d bytes sha256 %s, the agent sent %d bytes sha256 %s", f.n, got, res.Size, res.SHA256)}
			} else {
				f.err = io.EOF
			}
		}
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Close ends the read.
func (f *FileReader) Close() error { return f.t.Close() }

// Call calls an agent RPC method with args (JSON-encoded; nil for none) and
// decodes its result into result (nil discards it). The reply is capped at
// MaxReply.
func (c *Client) Call(ctx context.Context, method string, args, result any) error {
	h := RPCHeader{Method: method}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("rpc %s: encode args: %w", method, err)
		}
		h.Args = raw
	}
	t, err := c.open(ctx, TagRPC, h)
	if err != nil {
		return err
	}
	defer t.Close()
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()
	var data bytes.Buffer
	if err := readResult(NewFrameReader(t), &data); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	if result == nil || data.Len() == 0 {
		return nil
	}
	if err := json.Unmarshal(data.Bytes(), result); err != nil {
		return fmt.Errorf("rpc %s: decode result: %w", method, err)
	}
	return nil
}
