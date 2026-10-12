package cli

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/sshalias"
)

// sshServerStandIn answers the ssh-server Exec with exit 0, noting whether
// the user's agent was being provided while it ran.
type sshServerStandIn struct {
	fleetgrpc.UnimplementedFleetServiceServer
	providing        *atomic.Int32
	providedDuringIt atomic.Bool
}

func (s *sshServerStandIn) Exec(stream grpc.BidiStreamingServer[fleetgrpc.ExecIn, fleetgrpc.ExecOut]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	s.providedDuringIt.Store(s.providing.Load() == 1)
	return stream.Send(&fleetgrpc.ExecOut{Msg: &fleetgrpc.ExecOut_Exit{Exit: &fleetgrpc.ExecExit{Code: 0}}})
}

// Through an armada remote with [ agent: on ], ssh-proxy provides this
// machine's ssh-agent while the editor's connection runs (as a remote
// `fleet shell` does), and stops once it ends; a remote with forwarding off,
// or the local daemon, gets none.
func TestSSHProxyProvidesTheAgentToTheRemote(t *testing.T) {
	const remote = "ssh://ben@devbox"
	for _, tc := range []struct {
		name    string
		url     string
		enabled bool
		want    bool
	}{
		{"agent on", remote, true, true},
		{"agent off", remote, false, false},
		{"local daemon", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, stopped := stubAgentForward(t, tc.enabled)
			var asked []string
			agentForwardState = func(_ context.Context, url string) (bool, error) {
				asked = append(asked, url)
				return tc.enabled && url == remote, nil
			}
			providing := new(atomic.Int32)
			runAgentProvider = func(ctx context.Context, _ fleetgrpc.FleetServiceClient, role string, report func(agentfwd.Status)) {
				started.Add(1)
				providing.Add(1)
				report(agentfwd.Status{State: agentfwd.StateActive})
				<-ctx.Done()
				providing.Add(-1)
				stopped.Add(1)
			}
			srv := &sshServerStandIn{providing: providing}
			lis := bufconn.Listen(1 << 20)
			gs := grpc.NewServer()
			fleetgrpc.RegisterFleetServiceServer(gs, srv)
			go func() { _ = gs.Serve(lis) }()
			t.Cleanup(gs.Stop)
			cc, err := grpc.NewClient("passthrough:///bufnet",
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cc.Close() })

			code, err := sshProxyOver(context.Background(), fleetgrpc.NewFleetServiceClient(cc), sshalias.Target{Fleet: "alpha", Instance: "web"}, tc.url,
				strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil || code != 0 {
				t.Fatalf("ssh-proxy = %d, %v", code, err)
			}
			if got := srv.providedDuringIt.Load(); got != tc.want {
				t.Fatalf("agent provided during the connection = %v, want %v (asked about %q)", got, tc.want, asked)
			}
			if started.Load() != stopped.Load() {
				t.Fatalf("the provider outlived the connection: started %d, stopped %d", started.Load(), stopped.Load())
			}
		})
	}
}
