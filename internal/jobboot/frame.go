package jobboot

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// A frame on the job socket is a type byte, a big-endian uint32 payload
// length, then the payload. Control frames carry JSON; FrameData carries raw
// workspace bytes.
type FrameType byte

// Frame types. Each side ignores none: an unknown type is a protocol error, so
// a newer peer is caught by the hello's version, not by a silent skip.
const (
	// FrameHello opens a connection both ways (Hello).
	FrameHello FrameType = 'H'
	// FrameWorkspace starts a workspace transfer, fleetd → job (empty).
	FrameWorkspace FrameType = 'W'
	// FrameData is the next chunk of the workspace tar, fleetd → job.
	FrameData FrameType = 'D'
	// FrameEnd ends a workspace transfer with its length and sha256,
	// fleetd → job (WorkspaceEnd).
	FrameEnd FrameType = 'E'
	// FrameProgress reports what the job is doing, job → fleetd (Progress).
	FrameProgress FrameType = 'P'
	// FrameResult is the job's result, job → fleetd (Result).
	FrameResult FrameType = 'R'
	// FrameAck acknowledges the result, fleetd → job (empty).
	FrameAck FrameType = 'A'
	// FrameError reports a failure on either side (ErrorMessage).
	FrameError FrameType = 'X'
	// FramePing keeps an idle connection's transport alive (empty). It is
	// not activity: the stall watchdog waits for real bytes.
	FramePing FrameType = 'K'
	// FrameBarrier asks fleetd to do something before the job goes on, job →
	// fleetd (Barrier): a clone's copier stops at one before its final delta,
	// for fleetd to freeze or stop the source.
	FrameBarrier FrameType = 'B'
	// FrameRelease answers a barrier, fleetd → job (Release); one with Wait
	// set only says fleetd is still at it (activity for the stall watchdog).
	FrameRelease FrameType = 'G'
)

// ProtoVersion is the jobboot protocol this fleet speaks.
const ProtoVersion = 1

// MaxFrame caps a frame's payload; workspace data is sent in chunks under it.
const MaxFrame = 1 << 20

const frameHeaderLen = 5

// ErrFrameTooLarge is a frame whose declared length exceeds MaxFrame.
var ErrFrameTooLarge = errors.New("jobboot frame larger than the protocol allows")

func (t FrameType) valid() bool {
	switch t {
	case FrameHello, FrameWorkspace, FrameData, FrameEnd, FrameProgress,
		FrameResult, FrameAck, FrameError, FramePing, FrameBarrier, FrameRelease:
		return true
	}
	return false
}

func (t FrameType) String() string {
	if t.valid() {
		return string(rune(t))
	}
	return fmt.Sprintf("0x%02x", byte(t))
}

// WriteFrame writes one frame in a single Write, so frames from writers that
// share a lock never interleave.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	if !t.valid() {
		return fmt.Errorf("write jobboot frame: unknown type %s", t)
	}
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:frameHeaderLen], uint32(len(payload)))
	copy(buf[frameHeaderLen:], payload)
	_, err := w.Write(buf)
	return err
}

// WriteJSON writes a control frame whose payload is v as JSON.
func WriteJSON(w io.Writer, t FrameType, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, data)
}

// ReadFrame reads the next frame. A stream that ends cleanly between frames
// returns io.EOF; one that ends inside a frame returns io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var header [frameHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	t := FrameType(header[0])
	if !t.valid() {
		return 0, nil, fmt.Errorf("read jobboot frame: unknown type %s", t)
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return t, payload, nil
}

// Hello opens a connection. fleetd sends its Proto; the job answers with its
// own and what it is doing.
type Hello struct {
	Proto int `json:"jobboot"`
	// Op and Gen name the job (job → fleetd).
	Op  string `json:"op,omitempty"`
	Gen int    `json:"gen,omitempty"`
	// Workspace is whether the job still wants a workspace tar (job →
	// fleetd); false once one was received, even on a later connection.
	Workspace bool `json:"workspace,omitempty"`
	// Done is whether the job has finished; its Result follows the hello.
	Done bool `json:"done,omitempty"`
}

// WorkspaceEnd closes a workspace transfer: what fleetd sent, for the job to
// check what it received against.
type WorkspaceEnd struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Progress is a step of the job, for fleetd's provisioning log.
type Progress struct {
	// Phase is the step ("copy", "workspace", "remap", ...); Detail says
	// more ("/usr", "312 files").
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	// Bytes and Files count what the phase has written so far.
	Bytes int64 `json:"bytes,omitempty"`
	Files int64 `json:"files,omitempty"`
	// Done marks the phase finished.
	Done bool `json:"done,omitempty"`
}

// Result is how the job ended.
type Result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Bytes is what the job wrote in all.
	Bytes int64 `json:"bytes,omitempty"`
	// Output is the job's own result document (the seed's manifest), as
	// JSON; its sha256 is the termination message's.
	Output json.RawMessage `json:"output,omitempty"`
}

// ErrorMessage is a FrameError's payload.
type ErrorMessage struct {
	Error string `json:"error"`
	// Workspace marks an error about the workspace transfer the sender just
	// made (a checksum mismatch, a tar the job could not extract): the job
	// discarded it and waits for another.
	Workspace bool `json:"workspace,omitempty"`
}

// Barrier is a point the job waits at for fleetd (FrameBarrier).
type Barrier struct {
	// Seq numbers the job's barriers from 1: a release answers one Seq, and
	// a barrier sent again on a newer connection keeps its Seq.
	Seq int `json:"seq"`
	// Name says what the job waits for ("final", "copied").
	Name string `json:"name"`
	// Detail says more, for fleetd's log.
	Detail string `json:"detail,omitempty"`
}

// Release is fleetd's answer to a barrier (FrameRelease).
type Release struct {
	Seq int `json:"seq"`
	// Wait: fleetd is still working on the barrier; another release follows.
	Wait bool `json:"wait,omitempty"`
	// OK: the job goes on. Otherwise Error says why fleetd refused, and the
	// job fails with it.
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}
