//go:build !nok8s

package sshtunnel_test

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/cli"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// TestClusterCLIOverArmadaSSH runs one `fleet cluster` session against a real
// fleetd twice, over its local socket and then as an Armada client over SSH
// (FLEET_SSH: the real ssh binary, the -L forward, the token-gated
// listener), and requires the same transcript: every subcommand and flag,
// and every field `set` takes, behaves for a remote daemon exactly as for a
// local one. Paths are the daemon host's in both runs.
func TestClusterCLIOverArmadaSSH(t *testing.T) {
	d := startSSHModeFleetd(t)
	fields := []string{
		"display_name=every-field", "extra_path=" + d.extra, "plugin_env=AWS_PROFILE", "plugin_env=AWS_REGION=eu-west-1",
		"storage_class=standard", "disk_size=80Gi", "auto_expand_disk=false", "snapshot_class=csi-hostpath-snapclass",
		"clone_method=snapshot", "clone_consistency=stop-source", "backup_workspace_on_stop=true", "node_local_ack=true",
		"shared_storage_class=nfs", "share_size=20Gi", "share_retention=retain",
		"cache_storage_class=fast", "cache_sizes.buildkit=30Gi", "cache_sizes.debcache=15Gi", "cache_sizes.imagecache=25Gi",
		"service_images.buildkit=moby/buildkit:v0.34.0", "service_images.buildkit_rootless=moby/buildkit:v0.34.0-rootless",
		"service_images.debcache=sameersbn/apt-cacher-ng:3.7", "service_images.imagecache=registry:3", "service_images.dind=docker:29-dind",
		"allow_shared_buildkit=false", "delete_cache_volumes_on_destroy=true", "dind_address_pool=10.200.0.0/16", "block_metadata=false",
		"resources.cpu_request=1", "resources.memory_request=3Gi", "resources.autopilot_memory=6Gi", "memory_limit=12Gi",
		"node_selector=pool=fleet", "tolerations=dedicated=fleet:NoExecute:60", "allow_privileged=true", "debug_capabilities=require",
		"runtime_class=sysbox-runc", "service_account=fleet-pods", "pod_overrides_yaml=priorityClassName: fleet\n", "protect_from_drain=true",
		"host_gateway=off", "max_instances=20", "max_total_disk=1Ti", "automation_fire_interval=45s", "automation_max_per_agent=2",
		"image.mode=registry", "image.registry=registry.example.com/team", "image.insecure_registry=true", "image.pull_secrets=regcred",
		"image.managed_pull_secret=false", "image.builder=fleet", "image.platform=linux/arm64", "image.retention=48h",
		"image.upload_pace_percent=50", "rebuild_export=tar",
	}
	session := [][]string{
		{"cluster", "contexts", "--kubeconfig", d.kubeconfig},
		{"cluster", "ls"},
		{"cluster", "add", "remote-ctx", "--kubeconfig", d.kubeconfig, "--create-namespace", "--namespace-label", "a=b"},
		{"cluster", "add", "remote-ctx", "--name", "remote", "--kubeconfig", d.kubeconfig, "-n", "team", "extra_path=" + d.extra},
		append([]string{"cluster", "set", "remote"}, fields...),
		// The test's result is the connection's health from here on, so the
		// listings below show it whatever the background probe is doing.
		{"cluster", "test", "every-field"},
		{"cluster", "ls"},
		{"cluster", "ls", "every-field"},
		{"cluster", "set", "remote-ctx", "max_instances=", "tolerations="},
		{"cluster", "ls", "remote-ctx"},
		{"cluster", "test", "--context", "remote-ctx", "--kubeconfig", d.kubeconfig, "-n", "other", "allow_privileged=false"},
		{"cluster", "gc", "every-field"},
		{"cluster", "gc", "every-field", "--apply"},
		{"cluster", "login", "every-field"},
		{"cluster", "rm", "every-field", "--forget"},
		{"cluster", "ls"},
	}
	// ids are random per add, and the timestamps differ between runs; an
	// add replies before or (rarely) after the first background probe.
	volatile := regexp.MustCompile(`remote-ctx-[a-z0-9]{6}|(created_at|updated_at)=\S+|; (checking|unreachable)\n`)
	run := func(transport string) string {
		t.Setenv(fleetclient.EnvSSH, "")
		if transport == "armada-ssh" {
			t.Setenv(fleetclient.EnvSSH, d.url())
		}
		var transcript strings.Builder
		for _, args := range session {
			ctx, cancel := context.WithTimeout(d.ctx, 90*time.Second)
			cmd := cli.NewRootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(ctx)
			cancel()
			fmt.Fprintf(&transcript, "$ fleet %s\n%s", strings.Join(args, " "), out.String())
			if err != nil {
				fmt.Fprintf(&transcript, "exit 1\n")
			}
		}
		return volatile.ReplaceAllString(transcript.String(), "<$1$2>")
	}
	local := run("local")
	remote := run("armada-ssh")
	if remote != local {
		t.Errorf("the session differs over Armada SSH:\n--- local\n%s\n--- armada-ssh\n%s", local, remote)
	}

	for _, want := range []string{
		// contexts lists the daemon host's kubeconfig.
		"remote-ctx  remote   team       token",
		// add refuses before connecting when the namespace cannot be made,
		"Error: creating namespace \"team\": ",
		// and connects (the remote never resolves a path).
		"Added cluster \"remote\" (<>): context remote-ctx, namespace team<",
		"Updated cluster \"every-field\": display_name, extra_path, plugin_env,",
		"\nallow_privileged=true\n", "\nextra_path=" + d.extra + "\n", "\ntolerations=dedicated=fleet:NoExecute:60\n",
		"\npod_overrides_yaml=\"priorityClassName: fleet\\n\"\n", "\nimage.upload_pace_percent=50\n",
		"Updated cluster \"every-field\": max_instances, tolerations\n",
		// An unreachable cluster fails its test with exit 1, saved or not.
		"\nunreachable: ", "$ fleet cluster test --context remote-ctx",
		"does not log in through a credential plugin (it uses token)", "Removed cluster \"every-field\"\n",
		"No cluster connections; add one with 'fleet cluster add <context>'.\n",
	} {
		if !strings.Contains(local, want) {
			t.Errorf("the session lacks %q:\n%s", want, local)
		}
	}
	if strings.Contains(local, remoteKubeToken) {
		t.Errorf("the session prints the kubeconfig token:\n%s", local)
	}
	if test := between(local, "$ fleet cluster test every-field", "$ fleet cluster ls"); !strings.Contains(test, "exit 1") {
		t.Errorf("the test of an unreachable cluster exited 0:\n%s", test)
	}
	// `field=` cleared those two and kept the rest.
	cleared := between(local, "$ fleet cluster ls remote-ctx", "$ fleet cluster test --context")
	if strings.Contains(cleared, "max_instances=") || strings.Contains(cleared, "tolerations=") || !strings.Contains(cleared, "\nmax_total_disk=1Ti\n") {
		t.Errorf("set max_instances= tolerations= left:\n%s", cleared)
	}

	// The SSH run went through FLEET_SSH: pointed at a port nothing serves,
	// the same command fails.
	t.Setenv(fleetclient.EnvSSH, "ssh://tester@127.0.0.1:1")
	ctx, cancel := context.WithTimeout(d.ctx, 90*time.Second)
	defer cancel()
	cmd := cli.NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"cluster", "ls"})
	if err := cmd.ExecuteContext(ctx); err == nil {
		t.Error("fleet cluster ls through an unreachable FLEET_SSH succeeded: the session never used it")
	}
}

// between is s from the first start to the next end.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s, end); j >= 0 {
		return s[:j]
	}
	return s
}
