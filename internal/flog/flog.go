// Package flog is fleet-man's application-wide event log.
//
// Major lifecycle actions across the codebase — fleet and instance
// create/destroy, instance start/stop/clone, server-side jobs, session
// create/kill/rename, port-forwards, browser opens, TUI start/stop, the
// remote-gateway tunnel's connects/drops — and every command run against a
// container write structured records here so the whole system's behavior can
// be traced from one place: ~/.fleet/fleet.log.
//
// This is deliberately distinct from the per-instance creation logs under
// ~/.fleet/logs/<fleet>-<instance>.log, which capture a single instance's
// provisioning output. fleet.log is the cross-cutting, append-only trail of
// what the fleet host process(es) did and when.
//
// fleet runs as several cooperating processes — the long-lived TUI, the
// detached _create-instance / _clone-instance children, and one-shot CLI
// commands — and they all append to the same file, so records from every
// process interleave. The file is opened with O_APPEND and each slog record
// is written in a single Write call, so individual log lines never tear
// across processes; each record carries the pid so one process's actions can
// be followed through the interleaving.
//
// Logging is best-effort: if the log file cannot be opened the logger
// discards everything rather than failing the action being logged, mirroring
// the swallow-and-continue policy used elsewhere (e.g. state.WriteWarn).
package flog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const logFileName = "fleet.log"

var (
	initOnce sync.Once
	logger   *slog.Logger
)

// Path returns the absolute path of the event log file (~/.fleet/fleet.log).
//
// The ~/.fleet base is intentionally recomputed here rather than imported
// from internal/state: state — and most of the tree — imports flog, so
// importing state back would create a cycle. The path is trivial and stable,
// so this small duplication is worth keeping flog dependency-free and
// importable from anywhere, including state itself.
func Path() string {
	return filepath.Join(os.Getenv("HOME"), ".fleet", logFileName)
}

// L returns the shared logger, initializing it on first use. It always
// returns a usable *slog.Logger: if the log file cannot be opened the logger
// writes to io.Discard so callers never have to nil-check or handle errors.
func L() *slog.Logger {
	initOnce.Do(initLogger)
	return logger
}

func initLogger() {
	var w io.Writer = io.Discard

	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		// O_APPEND so concurrent fleet processes interleave cleanly; each
		// slog record is one Write, which the OS appends atomically.
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			w = f
		}
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Render the top-level timestamp as a compact local time
			// ("2006-01-02 15:04:05.000") instead of slog's default
			// RFC3339-with-nanos, which is noisier to scan by eye.
			if len(groups) == 0 && a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	})

	logger = slog.New(handler).With("pid", os.Getpid())
}

// Info, Warn, and Error append a structured event to the log. The variadic
// args are slog-style alternating key/value pairs:
//
//	flog.Info("instance created", "fleet", fleetName, "instance", name)
//	flog.Error("instance provisioning failed", "fleet", f, "instance", i, "err", err)
//
// Keep keys consistent across call sites so the log stays greppable. The
// conventional keys are: fleet, instance, session, backend, container,
// branch, remote, from, to, port, ms, err, job, kind, warn, gateway, and
// publicURL.
func Info(msg string, args ...any)  { L().Info(msg, args...) }
func Warn(msg string, args ...any)  { L().Warn(msg, args...) }
func Error(msg string, args ...any) { L().Error(msg, args...) }

// MillisSince returns the whole milliseconds elapsed since start. Use it for
// the "ms" attribute on completion events so durations are logged as a plain
// integer count of milliseconds:
//
//	start := time.Now()
//	... do the work ...
//	flog.Info("instance created", "fleet", f, "instance", i, "ms", flog.MillisSince(start))
//
// Only attach "ms" to events that mark the END of work that takes a
// measurable amount of time (provisioning, container start/stop, deletes,
// session/exec round-trips). Instant actions and "…started" markers should
// be left without it.
func MillisSince(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// ContainerExec records a command run against a container's workspace, with
// how long it took (ms) and whether it errored. Each container command is
// logged exactly once — when it finishes — so there is no separate "issued"
// event. Backends wire it up as the completion callback on the *Cmd returned
// by ExecCommand (so the duration is measured around the caller's own
// Run/Output/CombinedOutput) and call it directly from the synchronous Exec.
//
// High-frequency polling and probe loops route through ExecCommandQuiet, whose
// *Cmd carries no callback, so they log nothing and never flood the trace.
func ContainerExec(backend, workspaceDir string, command []string, d time.Duration, err error) {
	if err != nil {
		Error("container exec", "backend", backend, "workspace", workspaceDir, "cmd", strings.Join(command, " "), "ms", d.Milliseconds(), "err", err)
		return
	}
	Info("container exec", "backend", backend, "workspace", workspaceDir, "cmd", strings.Join(command, " "), "ms", d.Milliseconds())
}
