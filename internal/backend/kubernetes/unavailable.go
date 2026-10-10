package kubernetes

import (
	"io"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// unavailable answers every Backend method with err: a build without the
// Kubernetes backend (-tags nok8s) hands it out for a Kubernetes record, and
// the backend's own handle embeds one for the methods this version of fleet
// does not provide on a cluster yet. Nothing in it does I/O.
type unavailable struct{ err error }

var _ backend.Backend = unavailable{}

func (u unavailable) Up(string, []backend.Mount) (*backend.UpResult, error) { return nil, u.err }
func (u unavailable) SupportsSharedMounts() bool                            { return false }
func (u unavailable) Clone(string, string, []backend.Mount) (*backend.UpResult, error) {
	return nil, u.err
}
func (u unavailable) SupportsClone() bool { return false }
func (u unavailable) Rebuild(string, string, []backend.Mount) (*backend.UpResult, error) {
	return nil, u.err
}
func (u unavailable) SupportsRebuild() bool       { return false }
func (u unavailable) Down(string) error           { return u.err }
func (u unavailable) Stop(string) error           { return u.err }
func (u unavailable) Start(string) error          { return u.err }
func (u unavailable) Exec(string, []string) error { return u.err }
func (u unavailable) ExecCommand(string, []string) *backend.Cmd {
	return backend.NewCmd(u.cmd(), nil)
}
func (u unavailable) ExecCommandQuiet(string, []string) *backend.Cmd {
	return backend.NewCmd(u.cmd(), nil)
}
func (u unavailable) CopyFile(string, io.Reader, string, int) error { return u.err }
func (u unavailable) Stats([]string) (map[string]*backend.ContainerStats, error) {
	return nil, u.err
}
func (u unavailable) Logs(string, bool) error                       { return u.err }
func (u unavailable) LogsCommand(string, bool) *exec.Cmd            { return u.cmd() }
func (u unavailable) CaptureAllSessions(string) backend.AllSessions { return backend.AllSessions{} }
func (u unavailable) ListSessions(string) string                    { return "" }
func (u unavailable) AgentToolProbe(string) (string, bool)          { return "", false }
func (u unavailable) RunScript(string, string) (string, error)      { return "", u.err }
func (u unavailable) EditorURI(string, string) (string, bool)       { return "", false }
func (u unavailable) PortForwardCommand(string, int, int) *exec.Cmd {
	return u.cmd()
}
func (u unavailable) ForwardStdioCommand(string, int) (*exec.Cmd, bool) { return nil, false }
func (u unavailable) SupportsMicSink() bool                             { return false }
func (u unavailable) MicSinkCommand(string) (*exec.Cmd, bool)           { return nil, false }
func (u unavailable) ResolveHostname(string) (string, bool)             { return "", false }
func (u unavailable) Status(string) backend.LiveStatus                  { return backend.LiveStatusUnknown }

// cmd is a command that fails with u.err when started: a caller that runs
// what a method returned gets the reason, not a missing program's error.
func (u unavailable) cmd() *exec.Cmd {
	return &exec.Cmd{Path: "fleet-kubernetes-unavailable", Args: []string{"fleet-kubernetes-unavailable"}, Err: u.err}
}

// notYet is what a method of the Kubernetes backend that has not landed
// yet answers with: say so, and name the part of the backend that is
// missing.
type notYet struct{ what string }

func (e notYet) Error() string {
	return "kubernetes:unavailable: " + e.what + " is not available on Kubernetes in this version of fleet yet"
}

// Code is the error's kubernetes: code.
func (e notYet) Code() string { return "kubernetes:unavailable" }
