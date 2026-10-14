package podwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BenjaminBenetti/fleet-man/internal/execas"
)

// ExecHeader is an exec stream's preamble header: a command to start, or (with
// Resume set) a held one to re-attach.
type ExecHeader struct {
	// Request is the command: whom it runs as, its argv, environment,
	// directory and TTY. A re-attach names only Class and User, which must be
	// the held command's own.
	execas.Request
	// Resumable asks the agent to hold the command when the stream's
	// transport is lost (no ABORT, no FIN), so a later stream re-attaches it.
	// Without it, losing the transport aborts the command.
	Resumable bool `json:"resumable,omitempty"`
	// Resume re-attaches the held stream with this resume id instead of
	// starting a command.
	Resume string `json:"resume,omitempty"`
	// PodUID, when set, is the pod the client means to reach: a held stream
	// of another pod is never re-attached.
	PodUID string `json:"pod_uid,omitempty"`
	// Stdout and Stderr are, on a re-attach, how many bytes of each the
	// client already has; the agent replays from there.
	Stdout int64 `json:"stdout,omitempty"`
	Stderr int64 `json:"stderr,omitempty"`
	// ProtoMin and ProtoMax are the client's protocol range, for a stream
	// that is not on a session (a direct-mode `pod-agent exec --stdio`).
	ProtoMin int `json:"proto_min,omitempty"`
	ProtoMax int `json:"proto_max,omitempty"`
}

// Validate checks a header the agent received.
func (h ExecHeader) Validate() error {
	if h.Resume != "" {
		if !ValidResumeID(h.Resume) {
			return errors.New("bad resume id")
		}
		if !h.Class.Valid() {
			return fmt.Errorf("unknown exec class %q", h.Class)
		}
		if h.Stdout < 0 || h.Stderr < 0 {
			return errors.New("bad resume offsets")
		}
		return nil
	}
	return h.Request.Validate()
}

// ValidResumeID reports whether id has a resume id's form: 128 random bits
// as 32 lowercase hex digits.
func ValidResumeID(id string) bool { return isLowerHex(id, 32) }

// isLowerHex reports whether s is n lowercase hex digits.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// OpenReply answers an exec preamble that starts a command.
type OpenReply struct {
	// Proto is the protocol version of a stream not on a session.
	Proto int `json:"proto,omitempty"`
	// ResumeID re-attaches the stream; set only for a resumable one.
	ResumeID string `json:"resume_id,omitempty"`
	PID      int    `json:"pid,omitempty"`
	// Error is why the command did not start, and Code the exit code
	// `docker exec` gives for that (125, 126, 127).
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
	// Gone answers a re-attach whose held stream no longer exists (the pod
	// restarted, its grace ran out, or it never existed here).
	Gone bool `json:"gone,omitempty"`
	// Refused is set when the stream was refused before it was served
	// (Refusal).
	Refused string `json:"refused,omitempty"`
}

// ResumeReply answers a re-attach that found its held stream.
type ResumeReply struct {
	// Stdin is how many bytes of input the command has taken; the client
	// resends what it wrote after that.
	Stdin int64 `json:"stdin"`
	// Stdout and Stderr are where the replay starts: the offsets the client
	// sent.
	Stdout int64 `json:"stdout"`
	Stderr int64 `json:"stderr"`
	PID    int   `json:"pid,omitempty"`
	// Proto is the protocol version of a stream not on a session.
	Proto int `json:"proto,omitempty"`
}

// ExitStatus is how a command ended: EXIT's payload on an exec stream.
type ExitStatus struct {
	// Code is the exit code; -1 when a signal killed the command.
	Code int `json:"code"`
	// Signal names the signal that killed it ("KILL", "TERM", ...).
	Signal string `json:"signal,omitempty"`
	// Error is set when the agent lost the command's status.
	Error string `json:"error,omitempty"`
}

// Resize is RESIZE's payload.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Signal is SIGNAL's payload: a signal name without "SIG".
type Signal struct {
	Signal string `json:"signal"`
}

// Signals a client may send.
var Signals = []string{"HUP", "INT", "QUIT", "KILL", "USR1", "USR2", "TERM", "CONT", "STOP", "TSTP", "WINCH"}

// Ping is PING's payload on a resumable exec: how many bytes the sender has
// taken in so far, so the other side can drop what it was keeping for a
// replay. An empty PING is a heartbeat only.
type Ping struct {
	Stdin  int64 `json:"stdin,omitempty"`
	Stdout int64 `json:"stdout,omitempty"`
	Stderr int64 `json:"stderr,omitempty"`
}

// FileWriteHeader writes one file: to a temp file beside Path, then renamed
// over it, as the exec user, once Size bytes with this SHA256 have arrived.
// The data follows as DATA frames and an EOF; the agent answers with EXIT
// (Result).
type FileWriteHeader struct {
	Path string      `json:"path"`
	Mode fs.FileMode `json:"mode"`
	Size int64       `json:"size"`
	// SHA256 is the data's hex sha256.
	SHA256 string `json:"sha256"`
	// Class and User are whom the file is written as (default: the exec
	// class's user); missing parent directories are created as them too.
	Class execas.Class `json:"class,omitempty"`
	User  string       `json:"user,omitempty"`
}

// FileReadHeader reads one file as the exec user. The agent answers with OPEN
// (FileInfo), the data as DATA frames, then EXIT (Result, with the sha256).
type FileReadHeader struct {
	Path  string       `json:"path"`
	Class execas.Class `json:"class,omitempty"`
	User  string       `json:"user,omitempty"`
	// Max refuses a file larger than this (0: no limit beyond the session's).
	Max int64 `json:"max,omitempty"`
}

// FileInfo is OPEN's payload on a file read.
type FileInfo struct {
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	Error   string      `json:"error,omitempty"`
	Code    string      `json:"code,omitempty"`
	Refused string      `json:"refused,omitempty"`
}

// Result ends a file or RPC stream (EXIT's payload there).
type Result struct {
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
	Size  int64  `json:"size,omitempty"`
	// SHA256 is the hex sha256 of the bytes written or read.
	SHA256 string `json:"sha256,omitempty"`
}

// Error codes in a FileInfo or Result.
const (
	// CodeTooLarge: the file is over what this session carries (SmallFile on
	// the agent session; use the bulk session) or over the read's Max.
	CodeTooLarge = "too_large"
	CodeNotFound = "not_found"
	CodeDenied   = "denied"
	CodeInvalid  = "invalid"
	// CodeMismatch: the bytes did not match the header's size or sha256.
	CodeMismatch = "mismatch"
	// CodeUnknownMethod: no RPC by that name.
	CodeUnknownMethod = "unknown_method"
	CodeFailed        = "failed"
)

// RPCHeader calls one method. The agent answers with the result's JSON as
// DATA frames (at most MaxReply in all), then EXIT (Result).
type RPCHeader struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// RemoteError is an error the agent reported for a stream.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return e.Message }

// remoteError turns an error reply into an error; nil when there is none.
func remoteError(code, msg string) error {
	if msg == "" && code == "" {
		return nil
	}
	if msg == "" {
		msg = code
	}
	return &RemoteError{Code: code, Message: msg}
}

// IsCode reports whether err is a RemoteError with code.
func IsCode(err error, code string) bool {
	var re *RemoteError
	return errors.As(err, &re) && re.Code == code
}
