package tui

import (
	"context"
	"net"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"google.golang.org/grpc"
)

type configReplyServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
	config *fleetgrpc.Config
}

func (*configReplyServer) Hello(_ context.Context, req *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	return &fleetgrpc.HelloReply{ServerVersion: req.GetClientVersion()}, nil
}

func (s *configReplyServer) GetConfig(context.Context, *fleetgrpc.GetConfigRequest) (*fleetgrpc.GetConfigReply, error) {
	return &fleetgrpc.GetConfigReply{Config: s.config}, nil
}

// Exercise the RPC read path: a daemon without the output group must not inherit
// the new on-by-default preference intended for current daemons' fresh profiles.
func TestFetchConfigLegacyOutputCompatibility(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(fleetclient.EnvGateway, "")
	t.Setenv(fleetclient.EnvSSH, "")
	t.Setenv(fleetclient.EnvToken, "")
	mutConnMu.Lock()
	previous := mutConn
	mutConn = nil
	mutConnMu.Unlock()
	t.Cleanup(func() {
		closeMutationConn()
		mutConnMu.Lock()
		mutConn = previous
		mutConnMu.Unlock()
	})

	for _, tt := range []struct {
		name   string
		config *fleetgrpc.Config
		want   configutil.OutputSettings
	}{
		{name: "absent config"},
		{name: "older daemon omits output", config: &fleetgrpc.Config{}},
		{
			name:   "current daemon auto on",
			config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{Enabled: true}},
			want:   configutil.OutputSettings{Enabled: true},
		},
		{
			name:   "current daemon saved off",
			config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{Client: "laptop", Device: "headphones"}},
			want:   configutil.OutputSettings{Client: "laptop", Device: "headphones"},
		},
		{
			name:   "current daemon selected device",
			config: &fleetgrpc.Config{Output: &fleetgrpc.OutputSettings{Enabled: true, Client: "desktop", Device: "speakers"}},
			want:   configutil.OutputSettings{Enabled: true, Client: "desktop", Device: "speakers"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			fleetgrpc.RegisterFleetServiceServer(server, &configReplyServer{config: tt.config})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			t.Cleanup(closeMutationConn)
			t.Setenv(fleetclient.EnvServer, listener.Addr().String())

			// Reload uses the same read path and cached connection as initial load.
			for read := range 2 {
				cfg, err := fetchConfigLegacy()
				if err != nil {
					t.Fatal(err)
				}
				if cfg.OutputSettings != tt.want {
					t.Errorf("read %d output = %+v, want %+v", read, cfg.OutputSettings, tt.want)
				}
				if cfg.AgentSettings.ToolSelection != configutil.AgentToolClaude || cfg.MicSettings.Enabled {
					t.Errorf("unrelated defaults changed: agent=%+v mic=%+v", cfg.AgentSettings, cfg.MicSettings)
				}
			}
		})
	}
}
