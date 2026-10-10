package podwire

import (
	"io"
	"sync"
)

// pipeBuffer is how much of a command's stdout or stderr a client buffers
// while its reader is not reading; past it the stream's frames wait, and yamux
// flow control holds the agent back.
const pipeBuffer = 256 << 10

// pipe is an in-memory pipe whose writer blocks while its buffer is full,
// unlike io.Pipe, so stdout and stderr each read at their own pace without one
// of them stalling the stream until the other is read.
type pipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	max    int
	err    error // the writer's end: what Read returns once buf is drained
	closed bool  // the reader's end: writes are discarded
}

func newPipe(max int) *pipe {
	p := &pipe{max: max}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Read reads what is buffered, waiting for data or the writer's end.
func (p *pipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && p.err == nil && !p.closed {
		p.cond.Wait()
	}
	if p.closed {
		return 0, io.EOF
	}
	if len(p.buf) == 0 {
		return 0, p.err
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	if len(p.buf) == 0 {
		p.buf = nil
	}
	p.cond.Broadcast()
	return n, nil
}

// Write buffers b, waiting while the buffer is full. Once the reader's end is
// closed it discards; once the writer's end is closed it fails.
func (p *pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	written := 0
	for len(b) > 0 {
		for len(p.buf) >= p.max && !p.closed && p.err == nil {
			p.cond.Wait()
		}
		if p.closed {
			return written + len(b), nil
		}
		if p.err != nil {
			return written, io.ErrClosedPipe
		}
		n := min(len(b), p.max-len(p.buf))
		p.buf = append(p.buf, b[:n]...)
		b = b[n:]
		written += n
		p.cond.Broadcast()
	}
	return written, nil
}

// closeWrite ends the writer's side: Read returns err (io.EOF when nil) once
// the buffer is drained. Only the first call counts.
func (p *pipe) closeWrite(err error) {
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// closeRead ends the reader's side: Read returns io.EOF from now on, and
// writes are discarded instead of waiting for a reader.
func (p *pipe) closeRead() {
	p.mu.Lock()
	p.closed = true
	p.buf = nil
	p.mu.Unlock()
	p.cond.Broadcast()
}
