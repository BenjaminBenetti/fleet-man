// Package podwire is the wire protocol between fleetd and the agent fleet runs
// inside every Kubernetes pod (`fleet pod-agent`, internal/podagent).
//
// fleetd keeps one long-lived, non-TTY exec per running pod (`fleet pod-agent
// serve`), and a second one opened on demand for bulk transfers (`serve --role
// bulk`). Each exec's stdin and stdout carry a yamux session; fleetd is the
// yamux client. Everything else rides yamux streams, so polls, scripts, PCM,
// small files and the in-pod sockets never cost an apiserver exec upgrade of
// their own.
//
// # Streams
//
// A stream starts with a preamble: one tag byte naming what the stream is
// (TagExec, TagFileWrite, ...), then a JSON header, length-prefixed. After the
// preamble both directions carry frames: a type byte, a length and a payload
// (DATA, STDERR, EOF, RESIZE, SIGNAL, EXIT, OPEN, PING, RESUME, ABORT). The
// preamble and frame encodings are fixed for every protocol version; what
// travels inside them is versioned by the handshake.
//
// The first stream fleetd opens is the control stream (TagControl). The agent
// answers its preamble with Hello (its protocol range, build, uid, arch, pod
// UID and whether it can switch users); fleetd answers with HelloAck, choosing
// the highest protocol version both speak. An agent of another build is
// accepted as long as the ranges overlap, and is restaged lazily. The control
// stream then carries the heartbeat (a PING frame every 15s each way), the
// agent's own warnings and errors (LOG messages, which fleetd writes to
// fleet.log, rate-limited, see LogLimiter) and fleetd's Close.
//
// # Exec streams
//
// On an exec stream EOF is a half-close of the command's stdin; ABORT, or the
// stream's FIN before the agent sent EXIT, aborts the command (its whole
// process group); EXIT carries the exit status. A resumable exec survives the
// loss of its transport (no ABORT, no FIN: the session or a direct exec died):
// the agent holds the command and its output, and a later stream re-attaches
// by the stream's random 128-bit resume id, replaying exactly what the client
// had not received (PING frames carry the acknowledgements).
//
// # Caps
//
// Headers are capped at MaxHeader and frames at MaxFrame. Replies a receiver
// buffers whole (RPC results, Client.Run output, Client.ReadFile) are capped
// at MaxReply; streamed exec and file data is flow-controlled by yamux and has
// no total cap, as `docker exec` has none. Each side caps how many streams of
// a kind the other may hold open, gives a stream HeaderDeadline to send its
// preamble, and backs off accepting after a stream breaks those rules.
//
// The package builds on every OS: fleetd runs on macOS too. It is linked into
// the pod build of fleet, so it imports nothing of the daemon's.
package podwire
