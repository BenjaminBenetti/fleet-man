package tui

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	tea "github.com/charmbracelet/bubbletea"
)

// kubeLoginHereDoneMsg: the login plugin [L] ran in this terminal has
// exited (err is its failure, or why it could not start).
type kubeLoginHereDoneMsg struct {
	owner kubeDialog
	err   error
}

// kubeLoginHereAvailable reports whether [L] Run login here is offered: only
// over the local socket, where this machine is the daemon host.
func kubeLoginHereAvailable() bool { return !fleetclient.IsRemote() }

// kubeLoginHereCommandFn builds the plugin run (a seam for tests).
var kubeLoginHereCommandFn = kubeLoginHereCommand

// kubeLoginHereCmd hands the terminal to c's login plugin, then reports back
// to owner.
func kubeLoginHereCmd(owner kubeDialog, c *fleetgrpc.KubeCluster) tea.Cmd {
	cmd, err := kubeLoginHereCommandFn(c)
	if err != nil {
		return func() tea.Msg { return kubeLoginHereDoneMsg{owner: owner, err: err} }
	}
	return execProcess(cmd, func(err error) tea.Msg { return kubeLoginHereDoneMsg{owner: owner, err: err} })
}
