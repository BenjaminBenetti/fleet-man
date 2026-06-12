package tui

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// armada_client.go is the TUI's persistence path for the fleet-armada registry
// (the list of remote fleets the user can switch to). Unlike every other RPC in
// client.go, these NEVER ride the env-selected connection: the registry lives
// on the user's own machine, so they dial the LOCAL daemon explicitly — even
// while the main connection points at a remote fleet.

// armadaLocalTimeout bounds one armada registry RPC. Longer than
// mutationTimeout because DialLocal may auto-spawn the local daemon first (a
// remote-booted TUI has never touched it). These run inside tea.Cmd
// goroutines, so the wait never blocks the Update loop.
const armadaLocalTimeout = 15 * time.Second

// fetchArmadaLocal loads the registered remote fleets from the LOCAL daemon.
// Package var so tests can stub the persistence seam.
var fetchArmadaLocal = func() ([]configutil.ArmadaRemote, error) {
	ctx, cancel := context.WithTimeout(context.Background(), armadaLocalTimeout)
	defer cancel()
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	reply, err := conn.Service().GetArmada(ctx, &fleetgrpc.GetArmadaRequest{})
	if err != nil {
		return nil, err
	}
	remotes := make([]configutil.ArmadaRemote, 0, len(reply.GetRemotes()))
	for _, r := range reply.GetRemotes() {
		remotes = append(remotes, configutil.ArmadaRemote{URL: r.GetUrl(), Token: r.GetToken()})
	}
	return remotes, nil
}

// saveArmadaLocal replaces the registry on the LOCAL daemon. Package var so
// tests can stub the persistence seam.
var saveArmadaLocal = func(remotes []configutil.ArmadaRemote) error {
	ctx, cancel := context.WithTimeout(context.Background(), armadaLocalTimeout)
	defer cancel()
	conn, err := fleetclient.DialLocal(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	out := make([]*fleetgrpc.ArmadaRemote, 0, len(remotes))
	for _, r := range remotes {
		out = append(out, &fleetgrpc.ArmadaRemote{Url: r.URL, Token: r.Token})
	}
	_, err = conn.Service().SetArmada(ctx, &fleetgrpc.SetArmadaRequest{Remotes: out})
	return err
}

// pingArmadaRemote runs one Hello round trip against a registered remote.
// Package var so tests can stub network probing.
var pingArmadaRemote = func(url, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), armadaLocalTimeout)
	defer cancel()
	_, err := fleetclient.Ping(ctx, url, token)
	return err
}
