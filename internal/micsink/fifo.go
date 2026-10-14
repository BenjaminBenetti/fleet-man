package micsink

import (
	"errors"
	"sync"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

// fifoPiece is the size of one write to the FIFO. Two properties matter: it is
// EVEN, so a dropped piece never splits a 16-bit sample and turns the rest of
// the recording into noise; and it is <= PIPE_BUF (4096 on Linux), so the kernel
// writes it atomically — all of it or, when the pipe is full, none (EAGAIN).
const fifoPiece = 3200

// The backlog cap. PulseAudio reads the pipe in real time, so whatever sits in
// it is latency: audio that arrives in a burst (a network stall on the way to
// the instance, then everything at once) would otherwise sit there for the
// rest of the recording, up to the pipe's 64 KiB (two seconds). Past
// backlogMax the OLDEST audio is discarded down to backlogKeep: live audio is
// worth nothing late, and the newest is what the recorder should hear next.
// Both are whole samples (even).
const (
	backlogMax  = mic.BytesPerSecond / 4      // 250 ms
	backlogKeep = mic.BytesPerSecond * 3 / 25 // 120 ms
)

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
	// carry is the stream's sample PHASE, not audio waiting to go out: when the
	// bytes seen so far are odd in number it holds the last one — the LOW half
	// of a 16-bit sample whose high half is the first byte of whatever arrives
	// next. It is therefore tracked for every byte that arrives (also while the
	// microphone is closed) and is NEVER discarded: not on a drop, not on a
	// drain. Everything written to — or dropped from — the pipe is then a whole
	// number of samples, so the gap a drop leaves is always even. Discard the
	// carry and the gap becomes odd, which is exactly what shifts the reader's
	// sample boundaries: every later sample comes out byte-swapped, i.e. loud
	// noise for the rest of the recording.
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
// does not fit. While closed it only keeps the sample phase (see carry).
func (f *fifo) write(pcm []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Phase bookkeeping first, unconditionally.
	if len(f.carry) > 0 {
		pcm = append(f.carry, pcm...)
		f.carry = nil
	}
	if len(pcm)%2 == 1 {
		f.carry = []byte{pcm[len(pcm)-1]}
		pcm = pcm[:len(pcm)-1]
	}
	if !f.open {
		return // pcm is whole samples: dropping it keeps the phase
	}
	pcm = f.capLocked(pcm)
	for len(pcm) > 0 {
		piece := pcm[:min(fifoPiece, len(pcm))]
		_, err := syscall.Write(f.fd, piece)
		if errors.Is(err, syscall.EINTR) {
			continue // nothing was written; retry this same piece
		}
		if err != nil {
			// EAGAIN: the pipe is full because nothing is consuming. Drop the
			// rest of this buffer rather than spin. What is dropped is whole
			// samples (pieces are even), and the carry STAYS: see carry.
			return
		}
		pcm = pcm[len(piece):]
	}
}

// capLocked holds the pipe's backlog to backlogMax: when pcm would take it
// past that, the oldest audio goes first (out of the pipe, then off the front
// of pcm) until backlogKeep is left. Everything discarded is whole samples, so
// the phase holds. A pipe whose fill cannot be asked is left as it is (the
// full pipe's drop below still bounds it).
func (f *fifo) capLocked(pcm []byte) []byte {
	queued, err := pipeQueued(f.fd)
	if err != nil || queued < 0 || queued+len(pcm) <= backlogMax {
		return pcm
	}
	drop := queued + len(pcm) - backlogKeep
	fromPipe := min(drop, queued) &^ 1
	if fromPipe > 0 {
		f.discardLocked(fromPipe)
	}
	fromPCM := drop - fromPipe
	fromPCM += fromPCM % 2
	return pcm[min(fromPCM, len(pcm)):]
}

// discardLocked reads up to n bytes (even) off the front of the pipe: the
// oldest audio in it.
func (f *fifo) discardLocked(n int) {
	buf := make([]byte, min(n, 64*1024))
	for n > 0 {
		got, err := syscall.Read(f.fd, buf[:min(n, len(buf))])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if got <= 0 || err != nil {
			return
		}
		n -= got
	}
}

// drainMaxBytes bounds one drain. The pipe holds 64 KiB; the cap only matters if
// something else keeps writing to it, which must not be able to spin this loop
// forever while it holds the lock.
const drainMaxBytes = 4 << 20

// drainLocked empties the pipe. It does not touch carry: what sits in the pipe
// is whole samples, so discarding it leaves the phase where it was.
func (f *fifo) drainLocked() {
	buf := make([]byte, 64*1024)
	for drained := 0; drained < drainMaxBytes; {
		n, err := syscall.Read(f.fd, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if n <= 0 || err != nil {
			return
		}
		drained += n
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
