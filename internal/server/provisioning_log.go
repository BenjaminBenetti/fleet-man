package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/redact"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// provisioning_log.go is the sink a provisioning job (create, clone, rebuild)
// attaches to its ctx with backend.WithJobLog. Everything the run writes to it
// or reports through it lands twice:
//
//   - in the instance's creation log on this host
//     (state.InstanceCreationLogPath, 0600), which the TUI's logs view shows.
//     Each job starts a fresh file, so it holds the latest provisioning only;
//   - on the job's event stream: output and warnings as JobLog events, steps
//     as JobProgress events, which also move the job's current step (what
//     GetState and Watch's ActiveJobs report, and an instance row shows).
//
// One line of the file is one event. Every line is redacted first
// (redact.EnvLine): the devcontainer CLI logs its docker run with each
// containerEnv value as -e KEY=VALUE, and none of those values may reach the
// disk or a client. A line is redacted whole, before it is cut into pieces, so
// a value split across two pieces is still hidden; and it is made valid UTF-8
// (proto strings must be, or the job stream fails to send it).

const (
	// provisioningLogMaxLine bounds one line of the creation log and one JobLog
	// event: a longer output line is passed on in pieces of this size, after it
	// was redacted whole.
	provisioningLogMaxLine = 16 << 10
	// provisioningLogMaxPending bounds the unfinished line held in memory. A
	// line that grows past it is passed on up to here, ending in
	// provisioningLogCut, and the rest of it, up to the next newline, is
	// dropped: a piece cut off from the start of its line can't be redacted.
	provisioningLogMaxPending = 1 << 20
)

// provisioningLogCut ends a line that was too long to keep whole.
const provisioningLogCut = " [line cut]"

// provisioningLog is a job's provisioning-log sink: an io.Writer for raw
// output (split into lines) and a backend.JobReporter for steps and warnings.
// It takes writes from several goroutines and never fails one, since commands
// a step runs are teed into it.
type provisioningLog struct {
	job *job

	mu       sync.Mutex
	file     *os.File // nil when the creation log could not be written; events still flow
	path     string
	partial  []byte // output after the last newline
	dropping bool   // the line in progress was cut; drop it up to its newline
	closed   bool
}

var _ backend.JobReporter = (*provisioningLog)(nil)

// openProvisioningLog starts the creation log of fleetName/instanceName for
// job j, headed with what the job does ("create", "rebuild", "clone of x").
// The file replaces the instance's previous log through a rename, so a run
// that is still winding down never writes into this one. A file that cannot be
// created is logged and skipped: the job's events still carry everything.
func openProvisioningLog(j *job, fleetName, instanceName, what string) *provisioningLog {
	l := &provisioningLog{job: j, path: state.InstanceCreationLogPath(fleetName, instanceName)}
	f, err := createLogFile(l.path)
	if err != nil {
		flog.Warn("creation log unavailable", "fleet", fleetName, "instance", instanceName, "path", l.path, "err", err)
	} else {
		l.file = f
	}
	l.mu.Lock()
	l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_INFO, "",
		fmt.Sprintf("==> fleet %s: %s/%s at %s", what, fleetName, instanceName, time.Now().Format(time.RFC3339)), "")
	l.mu.Unlock()
	return l
}

// continueProvisioningLog is openProvisioningLog for a job that carries on
// from an earlier one's log (the rollback of a rebuild a daemon restart
// interrupted): its lines are appended to the instance's creation log.
func continueProvisioningLog(j *job, fleetName, instanceName, what string) *provisioningLog {
	l := &provisioningLog{job: j, path: state.InstanceCreationLogPath(fleetName, instanceName)}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err == nil {
		l.file, err = os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			l.file = nil
			flog.Warn("creation log unavailable", "fleet", fleetName, "instance", instanceName, "path", l.path, "err", err)
		}
	}
	l.mu.Lock()
	l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_INFO, "",
		fmt.Sprintf("==> fleet %s: %s/%s at %s", what, fleetName, instanceName, time.Now().Format(time.RFC3339)), "")
	l.mu.Unlock()
	return l
}

// createLogFile creates path afresh, 0600, in a 0700 directory when it has
// to make one: a temp file in the same directory renamed over path.
func createLogFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// Write takes raw output and passes on each complete line. It never fails.
func (l *provisioningLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return len(p), nil
	}
	for rest := p; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			l.appendLocked(rest)
			break
		}
		l.appendLocked(rest[:i])
		if !l.dropping {
			l.outputLocked(l.partial)
		}
		l.partial = l.partial[:0]
		l.dropping = false
		rest = rest[i+1:]
	}
	return len(p), nil
}

// appendLocked adds output to the unfinished line, keeping it within
// provisioningLogMaxPending: a terminal line redrawn in place (a progress
// bar) keeps only what follows its last carriage return, as outputLocked
// would, and a line still too long is passed on cut (see
// provisioningLogMaxPending).
func (l *provisioningLog) appendLocked(b []byte) {
	if l.dropping {
		return
	}
	l.partial = append(l.partial, b...)
	if len(l.partial) <= provisioningLogMaxPending {
		return
	}
	if i := bytes.LastIndexByte(l.partial[:len(l.partial)-1], '\r'); i >= 0 {
		l.partial = l.partial[:copy(l.partial, l.partial[i+1:])]
	}
	if len(l.partial) <= provisioningLogMaxPending {
		return
	}
	l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_INFO, "", carriageReturned(l.partial[:provisioningLogMaxPending]), provisioningLogCut)
	l.partial = l.partial[:0]
	l.dropping = true
}

// Step records that the run entered step: a "==> label: detail" line and a
// JobProgress event (which also becomes the job's current step).
func (l *provisioningLog) Step(step fleet.JobStep, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.flushPartialLocked()
	line := strings.ToValidUTF8(redact.EnvLine(backend.StepLine(step, detail)), "\uFFFD")
	l.writeFileLocked("==> " + line)
	l.job.emit(&fleetgrpc.JobEvent{JobId: l.job.summary.GetJobId(), Event: &fleetgrpc.JobEvent_Progress{Progress: &fleetgrpc.JobProgress{
		Step:    protoconv.JobStepToProto(step),
		Message: line,
	}}})
}

// Warn records a non-fatal warning, one WARN event and "warning: " line per
// line of message.
func (l *provisioningLog) Warn(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.flushPartialLocked()
	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_WARN, "warning: ", line, "")
	}
}

// finish ends the log with the job's outcome — its error, or a DONE step —
// and closes the file. Writes after it are dropped.
func (l *provisioningLog) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.flushPartialLocked()
	if err != nil {
		for _, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
			l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_ERROR, "error: ", line, "")
		}
	} else {
		label := fleet.StepDone.Label()
		l.writeFileLocked("==> " + label)
		l.job.emit(&fleetgrpc.JobEvent{JobId: l.job.summary.GetJobId(), Event: &fleetgrpc.JobEvent_Progress{Progress: &fleetgrpc.JobProgress{
			Step:     protoconv.JobStepToProto(fleet.StepDone),
			Message:  label,
			StepDone: true,
		}}})
	}
	l.closed = true
	if l.file != nil {
		if cerr := l.file.Close(); cerr != nil {
			flog.Warn("creation log close failed", "path", l.path, "err", cerr)
		}
		l.file = nil
	}
}

// outputLocked passes on one line of raw output.
func (l *provisioningLog) outputLocked(raw []byte) {
	l.lineLocked(fleetgrpc.LogLevel_LOG_LEVEL_INFO, "", carriageReturned(raw), "")
}

// carriageReturned is a raw output line as a terminal leaves it: a carriage
// return redraws the line in place, so only what follows the last one is kept.
func carriageReturned(raw []byte) string {
	line := strings.TrimSuffix(string(raw), "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return line
}

// flushPartialLocked passes on an unfinished line before a step, warning or
// the end, so the log keeps the order things happened in.
func (l *provisioningLog) flushPartialLocked() {
	if len(l.partial) > 0 {
		l.outputLocked(l.partial)
		l.partial = l.partial[:0]
	}
}

// lineLocked redacts line whole, makes it valid UTF-8 and adds suffix, then
// passes it on in pieces of at most provisioningLogMaxLine bytes (cut between
// characters): each piece is a line of the file behind filePrefix and a JobLog
// event of level.
func (l *provisioningLog) lineLocked(level fleetgrpc.LogLevel, filePrefix, line, suffix string) {
	line = strings.ToValidUTF8(redact.EnvLine(line), "\uFFFD") + suffix
	for {
		piece := line
		if len(piece) > provisioningLogMaxLine {
			end := provisioningLogMaxLine
			for end > 0 && !utf8.RuneStart(line[end]) {
				end--
			}
			piece = line[:end]
		}
		l.writeFileLocked(filePrefix + piece)
		l.job.emit(&fleetgrpc.JobEvent{JobId: l.job.summary.GetJobId(), Event: &fleetgrpc.JobEvent_Log{Log: &fleetgrpc.JobLog{
			Level: level,
			Line:  piece,
			At:    timestamppb.Now(),
		}}})
		line = line[len(piece):]
		if line == "" {
			return
		}
	}
}

// writeFileLocked appends one line to the creation log. A failed write is
// logged once and ends the file; the events go on.
func (l *provisioningLog) writeFileLocked(line string) {
	if l.file == nil {
		return
	}
	if _, err := l.file.WriteString(line + "\n"); err != nil {
		flog.Warn("creation log write failed", "path", l.path, "err", err)
		_ = l.file.Close()
		l.file = nil
	}
}

// appendCreationLogError ends the creation log of a job that can no longer
// finish it (the daemon running it went away) with the reason, as finish
// would have. An instance without a creation log gets none.
func appendCreationLogError(fleetName, instanceName, reason string) {
	path := state.InstanceCreationLogPath(fleetName, instanceName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.WriteString("error: " + redact.EnvLine(reason) + "\n"); err != nil {
		flog.Warn("creation log write failed", "path", path, "err", err)
	}
}

// removeCreationLog deletes an instance's creation log once the instance is
// gone. Best-effort: a log that is already gone, or cannot be removed, is no
// reason to fail a destroy.
func removeCreationLog(fleetName, instanceName string) {
	_ = os.Remove(state.InstanceCreationLogPath(fleetName, instanceName))
}
