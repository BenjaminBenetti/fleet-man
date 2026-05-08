package tui

// ===========================================
// Backend Group
// ===========================================

// backendGroup holds container IDs grouped by backend type. Sessions
// no longer need to be tracked here — CaptureAllSessions discovers
// every tmux session inside each container at capture time.
type backendGroup struct {
	ids []string
}
