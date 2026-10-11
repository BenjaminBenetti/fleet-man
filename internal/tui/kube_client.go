package tui

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// kube_client.go is the TUI's seam onto the daemon's Kubernetes connection
// RPCs. Each call goes to whichever daemon the TUI is on (local, gateway or
// Armada SSH) and is served there exactly as over the local socket: remote
// Fleet is full control, so nothing here looks at fleetclient.IsRemote(). The
// one key that does is [L] Run login here (kube_login_here.go), because it
// runs the login plugin on THIS machine, which is the daemon host only over
// the local socket.
//
// They are package vars, like the mutation wrappers in client.go, so tests
// stub one RPC without a server. All of them run in tea.Cmd goroutines, never
// on the Update loop: a cluster call can take as long as its plugin.

const (
	// kubeRPCTimeout bounds the unary kube RPCs. The daemon itself bounds the
	// cluster reads behind a re-point or a namespace create at 30s.
	kubeRPCTimeout = 45 * time.Second
	// kubeTestTimeout bounds a streamed TestKubeCluster (the daemon's full
	// preflight gives up after 6 minutes: 3 for its checks, 3 for the
	// multi-node storage test of a StorageClass it has no result for).
	kubeTestTimeout = 7 * time.Minute
	// kubeDeepTestTimeout bounds a deep test: the full preflight, then the
	// throwaway-pod probes (the pull through the gateway and the network
	// probe: 5 more minutes at most).
	kubeDeepTestTimeout = kubeTestTimeout + 5*time.Minute
	// kubeLargeStorageTimeout is how much longer a test runs that may test
	// a StorageClass with a large minimum size (Filestore makes a whole file
	// server for its volume: the daemon gives that test 12 minutes, not 3).
	kubeLargeStorageTimeout = 9 * time.Minute
	// kubeRemoveTimeout bounds RemoveKubeCluster, which with
	// DESTROY_INSTANCES waits for every teardown.
	kubeRemoveTimeout = 10 * time.Minute
	// kubeLoginTimeout bounds a login run on the daemon (its own bound is 10
	// minutes: the time a user has to finish a device-code sign-in).
	kubeLoginTimeout = 11 * time.Minute
)

// kubeCall dials (or reuses) the mutation connection and runs fn under its
// own deadline.
func kubeCall(timeout time.Duration, fn func(context.Context, fleetgrpc.FleetServiceClient) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	return fn(ctx, conn.Service())
}

// listKubeContextsRemote lists the contexts of kubeconfig files on the daemon
// host (empty paths: fleetd's defaults).
var listKubeContextsRemote = func(paths []string) (*fleetgrpc.ListKubeContextsReply, error) {
	var reply *fleetgrpc.ListKubeContextsReply
	err := kubeCall(kubeRPCTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.ListKubeContexts(ctx, &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: paths})
		return err
	})
	return reply, err
}

// addKubeClusterRemote saves a new connection.
var addKubeClusterRemote = func(cluster *fleetgrpc.KubeCluster) (*fleetgrpc.KubeClusterStatus, error) {
	var reply *fleetgrpc.AddKubeClusterReply
	err := kubeCall(kubeRPCTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.AddKubeCluster(ctx, &fleetgrpc.AddKubeClusterRequest{Cluster: cluster})
		return err
	})
	return reply.GetCluster(), err
}

// updateKubeClusterRemote changes the fields one mask names, refusing to
// save over a record whose revision moved since rev was read.
var updateKubeClusterRemote = func(id string, cluster *fleetgrpc.KubeCluster, paths []string, rev int64) (*fleetgrpc.KubeClusterStatus, error) {
	var reply *fleetgrpc.UpdateKubeClusterReply
	err := kubeCall(kubeRPCTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.UpdateKubeCluster(ctx, &fleetgrpc.UpdateKubeClusterRequest{
			Id:               id,
			Cluster:          cluster,
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: paths},
			ExpectedRevision: rev,
		})
		return err
	})
	return reply.GetCluster(), err
}

// removeKubeClusterRemote disconnects a connection.
var removeKubeClusterRemote = func(id string, mode fleetgrpc.KubeRemoveMode) (*fleetgrpc.RemoveKubeClusterReply, error) {
	var reply *fleetgrpc.RemoveKubeClusterReply
	err := kubeCall(kubeRemoveTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.RemoveKubeCluster(ctx, &fleetgrpc.RemoveKubeClusterRequest{Id: id, Mode: mode})
		return err
	})
	return reply, err
}

// createKubeNamespaceRemote creates (or, dry_run, renders) a connection's
// namespace.
var createKubeNamespaceRemote = func(req *fleetgrpc.CreateKubeNamespaceRequest) (*fleetgrpc.CreateKubeNamespaceReply, error) {
	var reply *fleetgrpc.CreateKubeNamespaceReply
	err := kubeCall(kubeRPCTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.CreateKubeNamespace(ctx, req)
		return err
	})
	return reply, err
}

// kubeTestStream is the client end of a TestKubeCluster stream.
type kubeTestStream interface {
	Recv() (*fleetgrpc.TestKubeClusterEvent, error)
}

// openKubeTestRemote starts a streamed preflight of a saved connection (id)
// or an unsaved draft (cluster). ctx bounds and cancels the whole stream.
var openKubeTestRemote = func(ctx context.Context, req *fleetgrpc.TestKubeClusterRequest) (kubeTestStream, error) {
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Service().TestKubeCluster(ctx, req)
}

// kubeLoginStream is the client end of a LoginKubeCluster stream.
type kubeLoginStream interface {
	Recv() (*fleetgrpc.LoginKubeClusterEvent, error)
}

// openKubeLoginRemote runs a connection's login plugin on the daemon host,
// relaying its device codes.
var openKubeLoginRemote = func(ctx context.Context, id string) (kubeLoginStream, error) {
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Service().LoginKubeCluster(ctx, &fleetgrpc.LoginKubeClusterRequest{Id: id})
}

// getShareSyncRemote reports each fleet's shared-state sync, its shares'
// usage on each cluster included (GetShareSync, every fleet).
var getShareSyncRemote = func() (*fleetgrpc.GetShareSyncReply, error) {
	var reply *fleetgrpc.GetShareSyncReply
	err := kubeCall(kubeRPCTimeout, func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		var err error
		reply, err = svc.GetShareSync(ctx, &fleetgrpc.GetShareSyncRequest{})
		return err
	})
	return reply, err
}
