package server

import (
	"bufio"
	"context"
	"io"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// exec.go implements the interactive-backend RPCs that let clients reach a
// container without importing internal/backend directly (the P5 boundary).
//
// ResolveExecCommand is the TTY carve-out (exec.proto): the server returns the
// argv it WOULD run (built by the backend), and the LOCAL client execs it itself
// so the user's terminal is inherited. Exec (bidi) — for a remote client with no
// local backend — lives in exec_stream.go.

// resolveServerInstance loads the persisted record for fleet/instance, mapping
// absence to a clear NotFound (so a typo'd target fails fast for the client).
func resolveServerInstance(fleetName, instanceName string) (*fleet.Instance, error) {
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "fleet %q not found", fleetName)
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "instance %q not found in fleet %q", instanceName, fleetName)
	}
	return inst, nil
}

// ResolveExecCommand returns the argv (+ env) the server would exec to run argv
// inside the instance. Exec/ExecCommand/ExecCommandQuiet all build from the same
// rawExec, so ExecCommand's argv is exactly what an interactive `Exec` would run
// — the client execs it locally with inherited stdio.
func (s *service) ResolveExecCommand(_ context.Context, req *fleetgrpc.ResolveExecCommandRequest) (*fleetgrpc.ResolveExecCommandReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	cmd := backendutil.NewForInstance(inst, false).ExecCommand(inst.WorkspaceDir, req.GetArgv()).Cmd
	return &fleetgrpc.ResolveExecCommandReply{
		Argv: cmd.Args,
		Env:  envSliceToMap(cmd.Env),
	}, nil
}

// Logs streams an instance's container logs to the client. The server runs the
// backend's logs command and forwards stdout+stderr line-by-line as LogLine.
// follow keeps the stream open until the client cancels; the process is killed
// when the client goes away.
func (s *service) Logs(req *fleetgrpc.LogsRequest, stream fleetgrpc.FleetService_LogsServer) error {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return err
	}
	cmd := backendutil.NewForInstance(inst, false).LogsCommand(inst.ContainerID, req.GetFollow())
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		flog.Error("logs failed", "fleet", req.GetFleet(), "instance", req.GetInstance(), "err", err)
		return status.Errorf(codes.Internal, "start logs: %v", err)
	}
	flog.Info("logs opened", "fleet", req.GetFleet(), "instance", req.GetInstance(), "follow", req.GetFollow())

	ctx := stream.Context()
	finished := make(chan struct{})
	// Kill the logs process if the client disconnects (e.g. cancels a --follow).
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-finished:
		}
	}()
	// Reap the process and close the pipe so the scanner below sees EOF.
	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
		close(finished)
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if err := stream.Send(&fleetgrpc.LogLine{Line: sc.Text(), At: timestamppb.Now()}); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// ResolveLogsCommand returns the argv of the host-side command that prints the
// instance's container runtime logs (backend.LogsCommand). The client embeds it
// in its own pager script and runs it locally — the analog of ResolveExecCommand
// for the TUI's `l` logs view, which needs the command string (not the Logs
// stream) so it can interleave the host-side creation log + a "press Enter"
// pause in one shell.
func (s *service) ResolveLogsCommand(_ context.Context, req *fleetgrpc.ResolveLogsCommandRequest) (*fleetgrpc.ResolveLogsCommandReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	cmd := backendutil.NewForInstance(inst, false).LogsCommand(inst.ContainerID, false)
	return &fleetgrpc.ResolveLogsCommandReply{Argv: cmd.Args}, nil
}

// envSliceToMap converts a "K=V" env slice to the proto map. A nil slice (the
// common case — the command inherits the process environment) yields nil, so a
// local client that runs the argv likewise inherits its own environment.
func envSliceToMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}
