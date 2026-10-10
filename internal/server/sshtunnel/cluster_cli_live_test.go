//go:build !nok8s

package sshtunnel_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/BenjaminBenetti/fleet-man/internal/cli"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// cluster_cli_live_test.go drives `fleet cluster` against a real cluster:
//
//	FLEET_KUBE_TEST_CONTEXT=fleet-k8s go test -count=1 -run Live ./internal/server/sshtunnel
//
// with the context in ~/.kube/config (or $KUBECONFIG). It creates and deletes
// namespaces named fleet-it-p1-04-cli-*.

const liveCLINamespacePrefix = "fleet-it-p1-04-cli-"

// On a real cluster, over the local socket and over Armada SSH alike: `add
// --create-namespace` makes the namespace through fleetd, a plain namespace
// tests usable (exit 0) and a baseline-labelled one LIMITED, gc lists
// nothing to collect under a held Lease, and rm disconnects.
func TestLiveClusterCLI(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("FLEET_KUBE_TEST_CONTEXT is not set")
	}
	// Read before the daemon's temp HOME replaces this one.
	kubeconfig := kubeexec.DefaultKubeconfigPaths(os.Environ())[0]
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	d := startSSHModeFleetd(t)

	fleetCLI := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(d.ctx, 4*time.Minute)
		defer cancel()
		cmd := cli.NewRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		t.Logf("$ fleet %s\n%s", strings.Join(args, " "), out.String())
		return out.String(), err
	}
	for _, transport := range []string{"local", "armada-ssh"} {
		t.Run(transport, func(t *testing.T) {
			t.Setenv(fleetclient.EnvSSH, "")
			if transport == "armada-ssh" {
				t.Setenv(fleetclient.EnvSSH, d.url())
			}
			plain := liveCLINamespacePrefix + transport + "-plain"
			baseline := liveCLINamespacePrefix + transport + "-baseline"
			t.Cleanup(func() {
				for _, ns := range []string{plain, baseline} {
					_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
				}
			})

			out, err := fleetCLI(t, "cluster", "add", kctx, "--name", "plain", "--kubeconfig", kubeconfig, "-n", plain, "--create-namespace")
			if err != nil || !strings.Contains(out, "Created namespace:\n") || !strings.Contains(out, "app.kubernetes.io/managed-by: fleet") {
				t.Fatalf("add plain: %v\n%s", err, out)
			}
			out, err = fleetCLI(t, "cluster", "add", kctx, "--name", "baseline", "--kubeconfig", kubeconfig, "-n", baseline,
				"--create-namespace", "--namespace-label", "pod-security.kubernetes.io/enforce=baseline")
			if err != nil || !strings.Contains(out, "pod-security.kubernetes.io/enforce: baseline") {
				t.Fatalf("add baseline: %v\n%s", err, out)
			}
			ns, err := cs.CoreV1().Namespaces().Get(context.Background(), baseline, metav1.GetOptions{})
			if err != nil || ns.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
				t.Fatalf("the baseline namespace: %v %v", ns.GetLabels(), err)
			}

			out, err = fleetCLI(t, "cluster", "test", "plain")
			lines := strings.Split(strings.TrimSpace(out), "\n")
			last := lines[len(lines)-1]
			if err != nil || !(strings.HasPrefix(last, "ready") || strings.HasPrefix(last, "limited")) {
				t.Errorf("test plain = %v, last line %q", err, last)
			}
			out, err = fleetCLI(t, "cluster", "test", "baseline")
			lines = strings.Split(strings.TrimSpace(out), "\n")
			if last := lines[len(lines)-1]; err != nil || !strings.HasPrefix(last, "limited") {
				t.Errorf("test baseline = %v, last line %q", err, last)
			}
			if !strings.Contains(out, "admission privileged    fail") {
				t.Errorf("the baseline namespace admitted a privileged pod:\n%s", out)
			}

			out, err = fleetCLI(t, "cluster", "ls")
			if err != nil || !strings.Contains(out, "plain ") || !strings.Contains(out, "baseline ") {
				t.Errorf("ls: %v\n%s", err, out)
			}
			for _, args := range [][]string{{"cluster", "gc", "plain"}, {"cluster", "gc", "plain", "--apply"}} {
				out, err = fleetCLI(t, args...)
				if err != nil || !strings.Contains(out, "lease held by this daemon\n") || !strings.Contains(out, "no objects to collect") {
					t.Errorf("%v: %v\n%s", args, err, out)
				}
			}
			for _, name := range []string{"plain", "baseline"} {
				if out, err := fleetCLI(t, "cluster", "rm", name); err != nil || !strings.Contains(out, "Removed cluster") {
					t.Errorf("rm %s: %v\n%s", name, err, out)
				}
			}
		})
	}
}
