package micsink

import (
	"errors"
	"sync"
	"syscall"
)

// fifoPiece is the size of one write to the FIFO. Two properties matter: it is
// EVEN, so a dropped piece never splits a 16-bit sample and turns the rest of
// the recording into noise; and it is <= PIPE_BUF (4096 on Linux), so the kernel
// writes it atomically — all of it or, when the pipe is full, none (EAGAIN).
const fifoPiece = 3200

// fifo is the write end of the pipe-source's FIFO. It deliberately bypasses
// os.File: Go parks a goroutine writing to a full non-blocking pipe until there
// is room, but a microphone must never queue — when nobody is reading, audio is
// DROPPED, or the next recording would open with seconds of stale speech.
type fifo struct {
	mu sync.Mutex
	fd int
	// open gates write. It lives here, under mu, rather than with the caller:
	// the "is anyone recording?" check and the write it guards must be ONE
	// critical section with respect to the drain, or a buffer that passed the
	// check just before the recorder left lands in the pipe just after it was
	// drained — and opens the next recording with a second of stale speech.
	open bool
	// carry holds the odd byte of a read that split a sample, to be prefixed to
	// the next write.
	carry []byte
}

func openFIFO(path string) (*fifo, error) {
	// O_RDWR rather than O_WRONLY: a write-only non-blocking open fails with
	// ENXIO whenever PulseAudio does not happen to hold the read end, and the
	// read half is what drain uses. Holding it is otherwise harmless — it is
	// only ever read while the source is idle (nothing else is consuming).
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &fifo{fd: fd}, nil
}

// setOpen opens or closes the microphone. Closing also discards everything
// sitting in the FIFO, atomically with respect to write: PulseAudio stops
// reading the pipe the moment the source goes idle, so the tail of one
// recording would otherwise be the first thing the next recorder hears.
func (f *fifo) setOpen(open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = open
	if !open {
		f.drainLocked()
	}
}

// write feeds pcm to the source while the microphone is open, dropping whatever
// does not fit. While closed it is a no-op.
func (f *fifo) write(pcm []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return
	}
	if len(f.carry) > 0 {
		pcm = append(f.carry, pcm...)
		f.carry = nil
	}
	if len(pcm)%2 == 1 {
		f.carry = []byte{pcm[len(pcm)-1]}
		pcm = pcm[:len(pcm)-1]
	}
	for len(pcm) > 0 {
		piece := pcm[:min(fifoPiece, len(pcm))]
		_, err := syscall.Write(f.fd, piece)
		if errors.Is(err, syscall.EINTR) {
			continue // nothing was written; retry this same piece
		}
		if err != nil {
			// EAGAIN: the pipe is full because nothing is consuming. Drop the
			// rest of this buffer rather than spin — INCLUDING the odd byte
			// stashed from its tail. That byte is the first half of a sample
			// whose second half arrives next; prefixing it to the next buffer
			// after everything before it was dropped would shift every later
			// sample by one byte, and the shift never heals: the rest of the
			// utterance would be loud noise, not a dropout.
			f.carry = nil
			return
		}
		pcm = pcm[len(piece):]
	}
}

func (f *fifo) drainLocked() {
	f.carry = nil
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(f.fd, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if n <= 0 || err != nil {
			return
		}
	}
}

func (f *fifo) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drain on the way out too: a sink that exits while a recording is live
	// (the last provider leaving) would otherwise leave captured speech in the
	// pipe, and no later sink may come along to clear it before a recorder does.
	if f.fd < 0 {
		return
	}
	f.open = false
	f.drainLocked()
	_ = syscall.Close(f.fd)
	f.fd = -1
}
