package backend

// FolderExecer is an optional capability for a backend that has a separate
// lane for bulk transfers: the `fleet copy` server handler runs a folder's
// `tar` (in or out) through FolderExecCommand instead of ExecCommand, so a
// large directory transfer does not hold up the latency-sensitive traffic of
// the backend's main channel. A backend without a separate lane does not
// implement it, and folder copies fall back to ExecCommand.
//
// The returned *Cmd behaves exactly like ExecCommandQuiet's (no event-log
// entry): the copy handler drives it with Start/Wait and pipes, and already
// logs the copy's outcome itself.
type FolderExecer interface {
	FolderExecCommand(workspaceDir string, command []string) *Cmd
}
