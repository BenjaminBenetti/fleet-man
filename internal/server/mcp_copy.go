package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// mcp_copy.go adds fleet_copy, the MCP form of scp-style `fleet copy`: a file
// or directory between the daemon host and an instance, or between two
// instances. The daemon runs the copy itself — the same fleetclient engine the
// CLI and the TUI run, over the same CopyFile/CopyInto handlers — so the bytes
// move between the endpoints and never pass through MCP. The daemon host's disk
// is the engine's "local" side.
//
// Endpoints are explicit {fleet, instance, path} objects, like every other
// fleet MCP tool, rather than the CLI's `[fleet/]instance:path` strings: there
// is no cwd to infer a fleet from or to anchor a relative host path to.

// mcpCopyEndpoint is one side of a fleet_copy.
type mcpCopyEndpoint struct {
	Fleet    string `json:"fleet,omitempty" jsonschema:"fleet of the instance; leave fleet and instance out for a path on the daemon host"`
	Instance string `json:"instance,omitempty" jsonschema:"instance the path is in; leave fleet and instance out for a path on the daemon host"`
	Path     string `json:"path" jsonschema:"in an instance: absolute, or relative to its workspace folder; on the daemon host: absolute, or starting with ~/"`
}

type FleetCopyInput struct {
	Src mcpCopyEndpoint `json:"src" jsonschema:"what to copy: a file or a directory"`
	Dst mcpCopyEndpoint `json:"dst" jsonschema:"where to copy it: an existing directory, or a path ending in /, keeps the source name inside it; any other path is the copy's own name"`
}

type FleetCopyOutput struct {
	Path  string `json:"path" jsonschema:"the absolute path the copy landed at, on the daemon host or in the destination instance"`
	Bytes int64  `json:"bytes"`
}

func (s *service) mcpCopy(ctx context.Context, _ *mcp.CallToolRequest, in FleetCopyInput) (*mcp.CallToolResult, FleetCopyOutput, error) {
	src, err := s.resolveMCPCopyEndpoint("src", in.Src)
	if err != nil {
		return nil, FleetCopyOutput{}, err
	}
	dst, err := s.resolveMCPCopyEndpoint("dst", in.Dst)
	if err != nil {
		return nil, FleetCopyOutput{}, err
	}
	// Stop the copy if the daemon shuts down or the MCP session closes, which
	// also unblocks a handler still streaming to an engine that gave up.
	cctx, cancel := mergeCtx(s.bgCtx, ctx)
	defer cancel()
	began := time.Now()
	res, err := fleetclient.Copy(cctx, inProcessCopyClient{s: s}, src, dst, fleetclient.HostLocalPolicy{})
	if err != nil {
		flog.Error("mcp copy", "src", copyEndpointLabel(src), "dst", copyEndpointLabel(dst), "ms", flog.MillisSince(began), "err", err)
		return nil, FleetCopyOutput{}, mcpErr(err)
	}
	flog.Info("mcp copy", "src", copyEndpointLabel(src), "dst", copyEndpointLabel(dst), "path", res.DestPath, "bytes", res.Written, "ms", flog.MillisSince(began))
	return nil, FleetCopyOutput{Path: res.DestPath, Bytes: res.Written}, nil
}

// resolveMCPCopyEndpoint checks one side of a copy and turns it into the
// engine's ResolvedEndpoint: an instance (both fleet and instance, running) or
// a daemon-host path (neither). which names the side in errors.
func (s *service) resolveMCPCopyEndpoint(which string, ep mcpCopyEndpoint) (fleetclient.ResolvedEndpoint, error) {
	if ep.Path == "" {
		return fleetclient.ResolvedEndpoint{}, fmt.Errorf("%s.path is required", which)
	}
	switch {
	case ep.Fleet == "" && ep.Instance == "":
		p, err := resolveHostCopyPath(ep.Path)
		if err != nil {
			return fleetclient.ResolvedEndpoint{}, fmt.Errorf("%s.path: %w", which, err)
		}
		return fleetclient.ResolvedEndpoint{Local: true, Path: p}, nil
	case ep.Fleet == "" || ep.Instance == "":
		return fleetclient.ResolvedEndpoint{}, fmt.Errorf("%s: give both fleet and instance for a path in an instance, or neither for a path on the daemon host", which)
	}
	if _, err := s.runningInstance(ep.Fleet, ep.Instance); err != nil {
		return fleetclient.ResolvedEndpoint{}, err
	}
	return fleetclient.ResolvedEndpoint{Fleet: ep.Fleet, Instance: ep.Instance, Path: ep.Path}, nil
}

// resolveHostCopyPath maps a daemon-host path onto the daemon's filesystem:
// `~` and `~/...` resolve against the daemon user's home, and anything else
// must be absolute — the daemon's cwd means nothing to the caller. A trailing
// slash (directory intent: keep the source name inside) survives.
func resolveHostCopyPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve ~: %w", err)
		}
		resolved := filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
		if strings.HasSuffix(p, "/") {
			resolved += string(filepath.Separator)
		}
		return resolved, nil
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q must be absolute or start with ~/ (a path on the daemon host)", p)
	}
	return p, nil
}

// copyEndpointLabel renders a resolved endpoint for log lines, in the CLI's
// syntax.
func copyEndpointLabel(ep fleetclient.ResolvedEndpoint) string {
	if ep.Local {
		return "host:" + ep.Path
	}
	return ep.Fleet + "/" + ep.Instance + ":" + ep.Path
}

// inProcessCopyClient serves the two RPCs the copy engine drives, CopyFile and
// CopyInto, by running this daemon's own handlers in-process — the same path
// the gRPC handlers take, with no socket hop (like streamCollector for Logs).
// The engine calls no other method; the embedded nil client is never reached.
//
// Each message is cloned as it crosses, as gRPC's marshalling would copy it:
// both sides reuse one read buffer for every data frame, so handing over the
// message itself would let the sender overwrite bytes the receiver has not
// written out yet.
type inProcessCopyClient struct {
	fleetgrpc.FleetServiceClient
	s *service
}

func (c inProcessCopyClient) CopyFile(ctx context.Context, req *fleetgrpc.CopyFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fleetgrpc.CopyFileChunk], error) {
	st := &copyFileInProcess{ctx: ctx, chunks: make(chan *fleetgrpc.CopyFileChunk), done: make(chan struct{})}
	go func() {
		st.err = c.s.CopyFile(req, copyFileServerSide{st: st})
		close(st.done)
	}()
	return st, nil
}

func (c inProcessCopyClient) CopyInto(ctx context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[fleetgrpc.CopyIntoChunk, fleetgrpc.CopyIntoReply], error) {
	st := &copyIntoInProcess{ctx: ctx, chunks: make(chan *fleetgrpc.CopyIntoChunk), closed: make(chan struct{}), done: make(chan struct{})}
	go func() {
		st.err = c.s.CopyInto(copyIntoServerSide{st: st})
		close(st.done)
	}()
	return st, nil
}

// copyFileInProcess is an in-process CopyFile stream: the handler's Sends hand
// chunks to the engine's Recvs, and its return ends the stream. The embedded
// nil ClientStream (Header/Trailer/CloseSend/...) is never called by the engine.
type copyFileInProcess struct {
	grpc.ClientStream
	ctx    context.Context
	chunks chan *fleetgrpc.CopyFileChunk
	done   chan struct{} // closed once the handler returned; err is set
	err    error
}

// Recv returns the next chunk, then io.EOF (or the handler's error) once the
// handler returned. The channel is unbuffered, so a returned handler has no
// chunk left in flight.
func (st *copyFileInProcess) Recv() (*fleetgrpc.CopyFileChunk, error) {
	select {
	case chunk := <-st.chunks:
		return chunk, nil
	case <-st.done:
		if st.err != nil {
			return nil, st.err
		}
		return nil, io.EOF
	case <-st.ctx.Done():
		return nil, st.ctx.Err()
	}
}

func (st *copyFileInProcess) Context() context.Context { return st.ctx }

// copyFileServerSide is the handler's half of a copyFileInProcess. The
// embedded nil ServerStream is never reached: the handler only calls Send and
// Context.
type copyFileServerSide struct {
	grpc.ServerStream
	st *copyFileInProcess
}

// Send fails once the engine is gone (ctx), as a gRPC Send fails once the
// client hung up, so the handler stops its exec.
func (ss copyFileServerSide) Send(chunk *fleetgrpc.CopyFileChunk) error {
	select {
	case ss.st.chunks <- proto.Clone(chunk).(*fleetgrpc.CopyFileChunk):
		return nil
	case <-ss.st.ctx.Done():
		return ss.st.ctx.Err()
	}
}

func (ss copyFileServerSide) Context() context.Context { return ss.st.ctx }

// copyIntoInProcess is an in-process CopyInto stream: the engine's Sends hand
// chunks to the handler's Recvs, CloseAndRecv is the client's half-close, and
// the handler's SendAndClose is the reply. The embedded nil ClientStream is
// never called by the engine.
type copyIntoInProcess struct {
	grpc.ClientStream
	ctx       context.Context
	chunks    chan *fleetgrpc.CopyIntoChunk
	closed    chan struct{} // the engine is done sending
	closeOnce sync.Once
	done      chan struct{} // closed once the handler returned; err and reply are set
	err       error
	reply     *fleetgrpc.CopyIntoReply
}

// Send hands one chunk to the handler. Once the handler returned it is io.EOF,
// as on a gRPC stream the server closed: CloseAndRecv carries the real status.
func (st *copyIntoInProcess) Send(chunk *fleetgrpc.CopyIntoChunk) error {
	select {
	case st.chunks <- proto.Clone(chunk).(*fleetgrpc.CopyIntoChunk):
		return nil
	case <-st.done:
		return io.EOF
	case <-st.ctx.Done():
		return st.ctx.Err()
	}
}

// CloseAndRecv half-closes the stream and waits for the handler's reply.
func (st *copyIntoInProcess) CloseAndRecv() (*fleetgrpc.CopyIntoReply, error) {
	st.closeOnce.Do(func() { close(st.closed) })
	select {
	case <-st.done:
	case <-st.ctx.Done():
		return nil, st.ctx.Err()
	}
	if st.err != nil {
		return nil, st.err
	}
	if st.reply == nil {
		return nil, errors.New("copy into: the server returned no reply")
	}
	return st.reply, nil
}

func (st *copyIntoInProcess) Context() context.Context { return st.ctx }

// copyIntoServerSide is the handler's half of a copyIntoInProcess. The
// embedded nil ServerStream is never reached: the handler only calls Recv,
// SendAndClose and Context.
type copyIntoServerSide struct {
	grpc.ServerStream
	st *copyIntoInProcess
}

// Recv returns the engine's next chunk, then io.EOF once it half-closed. The
// channel is unbuffered and the engine half-closes only after its last Send
// returned, so no chunk is left behind.
func (ss copyIntoServerSide) Recv() (*fleetgrpc.CopyIntoChunk, error) {
	select {
	case chunk := <-ss.st.chunks:
		return chunk, nil
	case <-ss.st.closed:
		return nil, io.EOF
	case <-ss.st.ctx.Done():
		return nil, ss.st.ctx.Err()
	}
}

// SendAndClose records the reply; CloseAndRecv reads it once the handler
// returned (done is closed after, so the write is visible).
func (ss copyIntoServerSide) SendAndClose(reply *fleetgrpc.CopyIntoReply) error {
	ss.st.reply = reply
	return nil
}

func (ss copyIntoServerSide) Context() context.Context { return ss.st.ctx }
