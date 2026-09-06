package server

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type sshStatus struct{ addr, err string }

func newTestSSHListener(t *testing.T, token string) (*sshListener, *[]sshStatus) {
	t.Helper()
	isolateFleetDir(t)
	var published []sshStatus
	l := &sshListener{
		newServer: func() (*grpc.Server, error) {
			authUnary, authStream := bearerAuthInterceptors(token)
			gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
			fleetgrpc.RegisterFleetServiceServer(gs, newService())
			return gs, nil
		},
		publish: func(addr, errMsg string) { published = append(published, sshStatus{addr, errMsg}) },
	}
	t.Cleanup(l.Stop)
	return l, &published
}

func helloAt(t *testing.T, addr, token string) error {
	t.Helper()
	cc, err := grpc.NewClient("dns:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	_, err = fleetgrpc.NewFleetServiceClient(cc).Hello(ctx, &fleetgrpc.HelloRequest{})
	return err
}

// TestSSHListenerLifecycle: enabling binds a loopback port, records it in
// ssh.port, serves the token-gated service; disabling stops serving and
// removes the hint. Re-enabling is a no-op while up.
func TestSSHListenerLifecycle(t *testing.T) {
	l, published := newTestSSHListener(t, "tok")

	l.Reconcile(true)
	if len(*published) != 1 || (*published)[0].addr == "" || (*published)[0].err != "" {
		t.Fatalf("enable published %+v", *published)
	}
	addr := (*published)[0].addr
	host, _, _ := net.SplitHostPort(addr)
	if host != "127.0.0.1" {
		t.Fatalf("listener must be loopback-only, got %s", addr)
	}
	data, err := os.ReadFile(fleetpaths.SSHPortPath())
	if err != nil {
		t.Fatalf("ssh.port not written: %v", err)
	}
	if _, port, _ := net.SplitHostPort(addr); string(data) != port {
		t.Fatalf("ssh.port = %q, want %s", data, port)
	}
	if _, err := strconv.Atoi(string(data)); err != nil {
		t.Fatalf("ssh.port not numeric: %q", data)
	}

	if err := helloAt(t, addr, "tok"); err != nil {
		t.Fatalf("Hello with token: %v", err)
	}
	if err := helloAt(t, addr, ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Hello without token: want Unauthenticated, got %v", err)
	}

	l.Reconcile(true) // no change
	if len(*published) != 1 {
		t.Fatalf("re-enable must be a no-op, published %+v", *published)
	}

	l.Reconcile(false)
	if len(*published) != 2 || (*published)[1] != (sshStatus{}) {
		t.Fatalf("disable published %+v", *published)
	}
	if _, err := os.Stat(fleetpaths.SSHPortPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ssh.port should be removed on disable, stat err = %v", err)
	}
	if err := helloAt(t, addr, "tok"); err == nil {
		t.Fatal("listener should be closed after disable")
	}
}

// TestSSHListenerServerBuildFailure: when the bearer token can't be loaded the
// listener publishes the error and opens nothing.
func TestSSHListenerServerBuildFailure(t *testing.T) {
	l, published := newTestSSHListener(t, "tok")
	l.newServer = func() (*grpc.Server, error) { return nil, errors.New("no token") }
	l.Reconcile(true)
	if len(*published) != 1 || (*published)[0].addr != "" || (*published)[0].err != "no token" {
		t.Fatalf("published %+v", *published)
	}
	if _, err := os.Stat(fleetpaths.SSHPortPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no ssh.port should exist after a failed enable")
	}
}
