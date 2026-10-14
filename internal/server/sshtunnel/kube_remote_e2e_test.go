//go:build !nok8s

package sshtunnel_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/server"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
)

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

const remoteKubeToken = "remote-kube-SECRET-token"

// TestKubeRPCsOverArmadaSSH drives a real fleetd in Remote Fleet via SSH
// mode the way an Armada client does: the real ssh binary runs the discovery
// script and the -L forward (the in-process SSH server stands in for the
// daemon host's sshd), and the kube RPCs go through the forward to the SSH
// listener. A connection is added from a kubeconfig on the daemon host,
// allow_privileged and extra_path are set, it is tested and removed, and no
// reply carries the kubeconfig's token.
func TestKubeRPCsOverArmadaSSH(t *testing.T) {
	d := startSSHModeFleetd(t)
	ctx, kubeconfig, extra, srv, fleetDir := d.ctx, d.kubeconfig, d.extra, d.srv, d.fleetDir
	m := sshtunnel.New(ctx)
	defer m.Close()
	rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
	defer rcancel()
	ep, err := m.Resolve(rctx, "ssh://tester@"+srv.Addr())
	if err != nil {
		t.Fatalf("Resolve over real ssh: %v", err)
	}
	conn, err := grpc.NewClient("dns:///"+ep.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(ep.Token)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := fleetgrpc.NewFleetServiceClient(conn)
	check := func(msg proto.Message) {
		t.Helper()
		data, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(remoteKubeToken)) {
			t.Errorf("a %T reply over Armada SSH carries the kubeconfig token", msg)
		}
	}

	contexts, err := c.ListKubeContexts(rctx, &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: []string{kubeconfig}})
	if err != nil {
		t.Fatalf("ListKubeContexts over Armada SSH: %v", err)
	}
	check(contexts)
	if len(contexts.GetContexts()) != 1 || contexts.GetContexts()[0].GetName() != "remote-ctx" || contexts.GetContexts()[0].GetAuth() != "token" {
		t.Fatalf("contexts = %v", contexts.GetContexts())
	}
	added, err := c.AddKubeCluster(rctx, &fleetgrpc.AddKubeClusterRequest{Cluster: &fleetgrpc.KubeCluster{Context: "remote-ctx", KubeconfigPaths: []string{kubeconfig}}})
	if err != nil {
		t.Fatalf("AddKubeCluster over Armada SSH: %v", err)
	}
	check(added)
	id := added.GetCluster().GetCluster().GetId()
	yes := true
	updated, err := c.UpdateKubeCluster(rctx, &fleetgrpc.UpdateKubeClusterRequest{
		Id: id, Cluster: &fleetgrpc.KubeCluster{AllowPrivileged: &yes, ExtraPath: []string{extra}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"allow_privileged", "extra_path"}},
	})
	if err != nil {
		t.Fatalf("UpdateKubeCluster over Armada SSH: %v", err)
	}
	check(updated)
	list, err := c.ListKubeClusters(rctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check(list)
	if len(list.GetClusters()) != 1 {
		t.Fatalf("clusters = %v", list.GetClusters())
	}
	rec := list.GetClusters()[0].GetCluster()
	if !rec.GetAllowPrivileged() || len(rec.GetExtraPath()) != 1 || rec.GetExtraPath()[0] != extra || rec.GetNamespace() != "team" || rec.GetKubeconfigPaths()[0] != kubeconfig {
		t.Errorf("stored over Armada SSH: %v", rec)
	}

	stream, err := c.TestKubeCluster(rctx, &fleetgrpc.TestKubeClusterRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	var result *fleetgrpc.KubeClusterStatus
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("TestKubeCluster over Armada SSH: %v", err)
		}
		check(ev)
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
	if result.GetState() != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE {
		t.Errorf("test of an unreachable cluster = %v %q", result.GetState(), result.GetMessage())
	}
	// The kube-exec shim's credential is the one RPC the remote does not
	// get (it execs through Exec instead); the daemon host's own socket
	// does, and is handed the connection's credential.
	if _, err := c.IssueKubeExecCredential(rctx, &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: id}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("IssueKubeExecCredential over Armada SSH = %v, want PermissionDenied", err)
	}
	local, err := grpc.NewClient("unix://"+filepath.Join(fleetDir, "fleet.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	cred, err := fleetgrpc.NewFleetServiceClient(local).IssueKubeExecCredential(rctx, &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: id})
	if err != nil || cred.GetCredential().GetBearerToken() != remoteKubeToken {
		t.Errorf("IssueKubeExecCredential on the daemon host's socket = %v, %v", cred, err)
	}

	removed, err := c.RemoveKubeCluster(rctx, &fleetgrpc.RemoveKubeClusterRequest{Id: id})
	if err != nil || !removed.GetRemoved() {
		t.Fatalf("RemoveKubeCluster over Armada SSH = %v, %v", removed, err)
	}
}

// sshModeFleetd is a real fleetd serving in Remote Fleet via SSH mode under
// a temp HOME, with an in-process SSH server standing in for the daemon
// host's sshd (its sessions run under the same HOME, so the discovery
// script finds this daemon).
type sshModeFleetd struct {
	ctx context.Context
	// fleetDir is the daemon's ~/.fleet; its fleet.sock is the daemon host's
	// own (local) socket.
	fleetDir string
	// kubeconfig is a kubeconfig on the daemon host whose context remote-ctx
	// (namespace team) reaches nothing and authenticates with
	// remoteKubeToken; extra is an existing directory for extra_path.
	kubeconfig, extra string
	srv               *sshtunnel.TestSSHServer
}

// url is the daemon's ssh:// Armada URL.
func (d *sshModeFleetd) url() string { return "ssh://tester@" + d.srv.Addr() }

func startSSHModeFleetd(t *testing.T) *sshModeFleetd {
	t.Helper()
	for _, tool := range []string{"ssh", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
	// Not t.TempDir(): fleetd's unix socket lives under HOME, and its path is
	// capped near 104 bytes.
	home, err := os.MkdirTemp("", "fleetd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv(agentsock.EnvOverride, "off")
	t.Setenv("SSH_AUTH_SOCK", "")
	fleetDir := filepath.Join(home, ".fleet")
	writeFile(t, filepath.Join(fleetDir, "config.json"), `{"remote_mcp_settings": {"fleet_enabled": true, "fleet_mode": "ssh"}}`)
	kubeconfig := filepath.Join(home, "kube", "remote-config")
	writeFile(t, kubeconfig, fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: remote
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: remote-ctx
  context: {cluster: remote, user: remote, namespace: team}
users:
- name: remote
  user: {token: %s}
`, remoteKubeToken))
	extra := filepath.Join(home, "sdk", "bin")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	t.Cleanup(func() {
		// The serve loop's first backup runs in the background under HOME:
		// let it finish before HOME goes away.
		for wait := time.Now().Add(10 * time.Second); time.Now().Before(wait); time.Sleep(20 * time.Millisecond) {
			if archives, _ := filepath.Glob(filepath.Join(fleetDir, "backup", "*", "*.tar.xz")); len(archives) > 0 {
				break
			}
		}
		cancel()
		select {
		case <-served:
		case <-time.After(20 * time.Second):
			t.Error("fleetd did not stop")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(fleetDir, "ssh.port")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fleetd never opened its SSH listener")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, clientPub, finish := sshtunnel.ClientSSHHome(t)
	srv := sshtunnel.StartTestSSHServer(t, clientPub, home)
	finish(srv)
	return &sshModeFleetd{ctx: ctx, fleetDir: fleetDir, kubeconfig: kubeconfig, extra: extra, srv: srv}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
