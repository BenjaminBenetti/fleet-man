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

// write feeds pcm to the source, dropping whatever does not fit.
func (f *fifo) write(pcm []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
		pcm = pcm[len(piece):]
		if _, err := syscall.Write(f.fd, piece); err != nil && !errors.Is(err, syscall.EINTR) {
			// EAGAIN: the pipe is full because nothing is consuming. Drop the
			// rest of this buffer rather than spin.
			return
		}
	}
}

// drain discards everything sitting in the FIFO. Called when the last recorder
// detaches (and at startup): PulseAudio stops reading the pipe the moment the
// source goes idle, so the tail of one recording would otherwise be the first
// thing the next recorder hears.
func (f *fifo) drain() {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	_ = syscall.Close(f.fd)
}
