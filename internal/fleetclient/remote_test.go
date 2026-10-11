package fleetclient

import (
	"context"
	"testing"
)

func TestCurrentRemoteFollowsTheEndpointPrecedence(t *testing.T) {
	for _, tc := range []struct {
		gw, ssh, srv, tok string
		want              Remote
		ok                bool
	}{
		{"", "", "", "", Remote{}, false},
		{"https://gw/grpc/s", "ssh://u@h", "h:1", " tk ", Remote{Kind: RemoteGateway, Address: "https://gw/grpc/s", Token: "tk"}, true},
		{"", "ssh://u@h", "h:1", "tk", Remote{Kind: RemoteSSH, Address: "ssh://u@h"}, true},
		{"", "", "h:1", "tk", Remote{Kind: RemoteServer, Address: "h:1"}, true},
	} {
		t.Setenv(EnvGateway, tc.gw)
		t.Setenv(EnvSSH, tc.ssh)
		t.Setenv(EnvServer, tc.srv)
		t.Setenv(EnvToken, tc.tok)
		got, ok := CurrentRemote()
		if got != tc.want || ok != tc.ok {
			t.Errorf("CurrentRemote() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
		}
		if ok != IsRemote() {
			t.Errorf("CurrentRemote and IsRemote disagree for %+v", tc)
		}
	}
}

func TestDialRemoteRefusesWhatItCannotDial(t *testing.T) {
	ctx := context.Background()
	if _, err := DialRemote(ctx, Remote{Kind: "carrier-pigeon", Address: "x"}); err == nil {
		t.Error("an unknown kind was dialled")
	}
	if _, err := DialRemote(ctx, Remote{Kind: RemoteGateway, Address: "ftp://gw/x"}); err == nil {
		t.Error("a malformed gateway URL was dialled")
	}
}
