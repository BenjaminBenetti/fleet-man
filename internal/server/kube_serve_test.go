//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// blackhole is an API server that accepts connections and never answers.
func blackhole(t *testing.T) (addr string, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			once.Do(func() { close(ch) })
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String(), ch
}

// A daemon whose connections are an API server that never answers and a
// login plugin that takes 30 seconds still answers Hello at once, every
// time: cluster I/O runs in the background after fleetd serves, and the
// kube RPCs answer from what it has (CHECKING). Shutdown is not held up
// either.
func TestServeAnswersWithAnUnreachableClusterAndASlowLoginPlugin(t *testing.T) {
	addr, accepted := blackhole(t)
	var marker string
	client, served := startServeSeeded(t, func() {
		home := os.Getenv("HOME")
		bin := filepath.Join(home, "bin")
		marker = filepath.Join(home, "plugin-started")
		writePluginScript(t, filepath.Join(bin, "slow-login"), fmt.Sprintf("touch %s\nsleep 30\necho '{}'", marker))
		kubeconfig := filepath.Join(home, ".kube", "config")
		writePluginScript(t, kubeconfig, "") // creates the directory
		if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: blackhole
  cluster: {server: "https://%s", insecure-skip-tls-verify: true}
- name: nowhere
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: blackhole
  context: {cluster: blackhole, user: token}
- name: slow
  context: {cluster: nowhere, user: slow}
users:
- name: token
  user: {token: t0ken}
- name: slow
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: slow-login, interactiveMode: Never}}
`, addr)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
			k.Clusters = append(k.Clusters,
				fleet.KubeCluster{ID: "blackhole-aaaaaa", Context: "blackhole", KubeconfigPaths: []string{kubeconfig}, Namespace: "fleet"},
				fleet.KubeCluster{ID: "slow-bbbbbb", Context: "slow", KubeconfigPaths: []string{kubeconfig}, Namespace: "fleet", ExtraPath: []string{bin}},
			)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	// The cluster I/O is under way: the API server has a connection and
	// the plugin is running.
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never dialed the unreachable cluster")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			r, _ := client.ListKubeClusters(context.Background(), &fleetgrpc.ListKubeClustersRequest{})
			t.Fatalf("the daemon never ran the login plugin: %v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Fresh dials, as a burst of CLI commands makes them: each answers well
	// within the client's liveness window.
	for i := 0; i < 10; i++ {
		conn, err := grpc.NewClient("unix://"+fleetpaths.SocketPath(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		c := fleetgrpc.NewFleetServiceClient(conn)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if _, err := c.Hello(ctx, &fleetgrpc.HelloRequest{}); err != nil {
			t.Fatalf("dial %d: Hello: %v", i, err)
		}
		reply, err := c.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
		cancel()
		if err != nil {
			t.Fatalf("dial %d: ListKubeClusters: %v", i, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("dial %d took %v", i, elapsed)
		}
		if len(reply.GetClusters()) != 2 {
			t.Fatalf("clusters = %v", reply.GetClusters())
		}
		for _, st := range reply.GetClusters() {
			if st.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING {
				t.Errorf("%s = %v while its first probe hangs, want CHECKING", st.GetCluster().GetId(), st.GetState())
			}
		}
		_ = conn.Close()
	}

	start := time.Now()
	shutdownAndWait(t, client, served)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("shutdown took %v with the probes hung", elapsed)
	}
	awaitFirstBackup(t)
}
