package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/devmeta"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/redact"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// editor.go is the daemon's half of opening VS Code on an instance (the
// TUI's `c`, `fleet code`). ResolveEditorLaunch picks how: a local caller
// opens a local devcontainer instance through the Dev Containers URI, as
// before; a caller on another machine, and every Kubernetes instance, go
// through VS Code Remote-SSH: OpenSSH runs `fleet ssh-proxy <alias>` on the
// editor's machine, which opens an Exec stream running `fleet ssh-server
// --stdio` in the instance. That Exec is an editor session: before it starts,
// the daemon writes the instance's EditorConfig (its merged customizations)
// to its control directory, and ssh-server's report of what it applied goes
// to the instance's creation log as well as to the caller.

// editorBackend is the backend handle an editor launch and an editor session
// use (a var so tests can stand one in).
var editorBackend = func(inst *fleet.Instance) backend.Backend {
	return backendutil.NewForInstance(inst, false)
}

// ResolveEditorLaunch says how the caller opens VS Code on the instance.
func (s *service) ResolveEditorLaunch(ctx context.Context, req *fleetgrpc.ResolveEditorLaunchRequest) (*fleetgrpc.ResolveEditorLaunchReply, error) {
	inst, err := resolveServerInstance(req.GetFleet(), req.GetInstance())
	if err != nil {
		return nil, err
	}
	switch inst.Backend {
	case fleet.BackendCoder:
		return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODER_CLI,
			Argv: []string{"coder", "open", "vscode", inst.ContainerID}}, nil
	case fleet.BackendCodespaces:
		return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_CODESPACES_CLI,
			Argv: []string{"gh", "codespace", "code", "-c", inst.ContainerID}}, nil
	}
	b := editorBackend(inst)
	if uri, ok := b.EditorURI(inst.WorkspaceDir, req.GetFleet()); ok && devContainerURI(uri) && !remoteCaller(ctx) {
		return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_DEVCONTAINER_URI,
			Argv: []string{"code", "--folder-uri", uri}}, nil
	}
	target, ok := b.(backend.EditorTarget)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "%s instances do not open in VS Code from another machine", inst.Backend)
	}
	if inst.Status != fleet.StatusRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "%s/%s is %s: start it first", req.GetFleet(), inst.Name, inst.Status)
	}
	info, err := target.EditorInfo(ctx, inst.ContainerID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read the instance's editor settings: %v", err)
	}
	if info.WorkspaceFolder == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s/%s has no workspace folder to open", req.GetFleet(), inst.Name)
	}
	return &fleetgrpc.ResolveEditorLaunchReply{Mode: fleetgrpc.EditorLaunchMode_EDITOR_LAUNCH_MODE_REMOTE_SSH,
		WorkspaceFolder: info.WorkspaceFolder}, nil
}

// devContainerURI reports whether an editor URI is the Dev Containers
// extension's, which only opens a container on the editor's own docker.
func devContainerURI(uri string) bool {
	return strings.HasPrefix(uri, "vscode-remote://dev-container+")
}

// isEditorSession reports whether an Exec runs the in-instance ssh-server
// (sshalias.ServerArgv), i.e. is one VS Code Remote-SSH connection.
func isEditorSession(argv []string) bool {
	return slices.Equal(argv, sshalias.ServerArgv())
}

// editorStaged remembers the containers whose fleet binary an editor
// session already staged (by ContainerID), so a connection after the first
// does not probe it again.
var editorStaged sync.Map

// prepareEditorSession readies inst, of fleetName, for an editor session:
// its staged fleet binary is the daemon's (it must know ssh-server), and its
// control directory holds its current EditorConfig, which ssh-server reads
// once VS Code's server appears. Best-effort: a failure costs the
// customizations, not the connection, and is logged.
func prepareEditorSession(ctx context.Context, fleetName string, inst *fleet.Instance) {
	b := editorBackend(inst)
	if _, done := editorStaged.Load(inst.ContainerID); !done {
		if _, err := fleetlaunch.EnsureFresh(b, inst.WorkspaceDir, nil); err != nil {
			flog.Warn("editor session: stage the fleet binary", "fleet", fleetName, "instance", inst.Name, "err", err)
		} else {
			editorStaged.Store(inst.ContainerID, true)
		}
	}
	data, err := editorConfigFor(ctx, b, inst)
	if err != nil {
		flog.Warn("editor session: no editor settings", "fleet", fleetName, "instance", inst.Name, "err", err)
		return
	}
	sink := controlSinkFor(fleetName, inst)
	if !sink.reaches() {
		flog.Warn("editor session: the instance has no control directory", "fleet", fleetName, "instance", inst.Name)
		return
	}
	if _, err := sink.write(devmeta.EditorConfigName, data, 0o644, time.Now()); err != nil {
		flog.Warn("editor session: write the editor settings", "fleet", fleetName, "instance", inst.Name, "err", err)
	}
}

// editorConfigFor is inst's EditorConfig, as the JSON its control file holds.
func editorConfigFor(ctx context.Context, b backend.Backend, inst *fleet.Instance) ([]byte, error) {
	target, ok := b.(backend.EditorTarget)
	if !ok {
		return nil, fmt.Errorf("%s instances have no editor settings", inst.Backend)
	}
	info, err := target.EditorInfo(ctx, inst.ContainerID)
	if err != nil {
		return nil, err
	}
	cfg, err := devmeta.EditorFromMetadata(info.Metadata)
	if err != nil {
		return nil, err
	}
	cfg.Gen = inst.ContainerID
	cfg.WorkspaceFolder = info.WorkspaceFolder
	return json.Marshal(cfg)
}

// editorStream is an editor session's stream, its error output also going
// to the instance's creation log.
type editorStream struct {
	backend.ExecStream
	stderr io.Reader
}

func (s editorStream) Stderr() io.Reader { return s.stderr }

// withCreationLog tees proc's error output (ssh-server's report: extensions
// installed, settings written, postAttachCommand's output) into log.
func withCreationLog(proc backend.ExecStream, log io.Writer) backend.ExecStream {
	return editorStream{ExecStream: proc, stderr: io.TeeReader(proc.Stderr(), log)}
}

// creationLogLine caps one line an editor session appends, and
// creationLogSession all it appends: what runs in the instance writes the
// stream, and the log is on the daemon's host.
const (
	creationLogLine    = 16 << 10
	creationLogSession = 1 << 20
)

// creationLogAppender appends complete lines, redacted, to an instance's
// creation log, one write per line (O_APPEND keeps concurrent sessions'
// lines whole), up to creationLogSession bytes. It never fails a write: the
// stream it tees must not break over the log.
type creationLogAppender struct {
	mu      sync.Mutex
	path    string
	buf     []byte
	written int
	cut     bool
}

func newCreationLogAppender(fleetName, instance string) *creationLogAppender {
	return &creationLogAppender{path: state.InstanceCreationLogPath(fleetName, instance)}
}

func (a *creationLogAppender) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf = append(a.buf, p...)
	for {
		i := bytes.IndexByte(a.buf, '\n')
		if i < 0 {
			break
		}
		a.appendLine(string(a.buf[:i]))
		a.buf = a.buf[i+1:]
	}
	if len(a.buf) > creationLogLine {
		a.appendLine(string(a.buf[:creationLogLine]) + " [line cut]")
		a.buf = a.buf[:0]
	}
	return len(p), nil
}

// Close appends what is left of an unfinished line.
func (a *creationLogAppender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) > 0 {
		a.appendLine(string(a.buf))
		a.buf = nil
	}
	return nil
}

func (a *creationLogAppender) appendLine(line string) {
	line = redact.EnvLine(strings.TrimRight(line, "\r"))
	if len(line) > creationLogLine {
		n := creationLogLine
		for n > 0 && !utf8.RuneStart(line[n]) {
			n--
		}
		line = line[:n] + " [line cut]"
	}
	line = strings.ToValidUTF8(line, "\uFFFD")
	if a.cut {
		return
	}
	if a.written+len(line)+1 > creationLogSession {
		a.cut = true
		line = "warning: this editor session's report was cut at 1 MiB"
	}
	a.written += len(line) + 1
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// SaveEditorEndpoint records a remote daemon an editor alias leads to
// (local-only; editor_endpoints.json), keyed by the alias's remote part.
func (s *service) SaveEditorEndpoint(_ context.Context, req *fleetgrpc.SaveEditorEndpointRequest) (*fleetgrpc.SaveEditorEndpointReply, error) {
	ep := protoconv.EditorEndpointFromProto(req.GetEndpoint())
	ep.Address = strings.TrimSpace(ep.Address)
	ep.Token = strings.TrimSpace(ep.Token)
	if !ep.Kind.Valid() || ep.Address == "" {
		return nil, status.Error(codes.InvalidArgument, "an editor endpoint needs a kind and an address")
	}
	remote := sshalias.RemoteHash(sshalias.RemoteKey(ep.Kind == state.EditorEndpointServer, ep.Address))

	s.muWrite.Lock()
	defer s.muWrite.Unlock()
	recs, err := state.LoadEditorEndpoints()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load editor endpoints: %v", err)
	}
	if prev, ok := recs.Endpoints[remote]; !ok || prev != ep {
		if recs.Endpoints == nil {
			recs.Endpoints = map[string]state.EditorEndpoint{}
		}
		recs.Endpoints[remote] = ep
		if err := state.SaveEditorEndpoints(recs); err != nil {
			return nil, status.Errorf(codes.Internal, "save editor endpoints: %v", err)
		}
		// Never the address or the token: either may be a secret's carrier.
		flog.Info("editor endpoint recorded", "remote", remote, "kind", string(ep.Kind))
	}
	return &fleetgrpc.SaveEditorEndpointReply{Remote: remote}, nil
}

// ResolveEditorEndpoint looks a recorded remote daemon up by an alias's
// remote part (local-only). An unknown one has no endpoint.
func (s *service) ResolveEditorEndpoint(_ context.Context, req *fleetgrpc.ResolveEditorEndpointRequest) (*fleetgrpc.ResolveEditorEndpointReply, error) {
	if !sshalias.ValidRemote(req.GetRemote()) {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not an alias's remote part", req.GetRemote())
	}
	recs, err := state.LoadEditorEndpoints()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load editor endpoints: %v", err)
	}
	ep, ok := recs.Endpoints[req.GetRemote()]
	if !ok {
		return &fleetgrpc.ResolveEditorEndpointReply{}, nil
	}
	return &fleetgrpc.ResolveEditorEndpointReply{Endpoint: protoconv.EditorEndpointToProto(ep)}, nil
}
