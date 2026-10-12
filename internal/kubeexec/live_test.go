//go:build !nok8s

package kubeexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// LiveContextEnv names the kube context of a real cluster the live tests of
// kubeexec and k8s/owner run against; unset, they skip. They read the
// daemon-default kubeconfig ($KUBECONFIG or ~/.kube/config); k8s/owner's
// creates one fleet-it-p1-01-owner-* namespace and deletes it.
const LiveContextEnv = "FLEET_KUBE_TEST_CONTEXT"

// A real kubeconfig context loads pinned, its client talks to the API
// server, and its credential's expiry is read.
func TestLiveConnection(t *testing.T) {
	kctx := os.Getenv(LiveContextEnv)
	if kctx == "" {
		t.Skip(LiveContextEnv + " is not set")
	}
	paths := DefaultKubeconfigPaths(os.Environ())
	contexts, err := ListContexts(paths)
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	var found *KubeContext
	for i := range contexts {
		if contexts[i].Name == kctx {
			found = &contexts[i]
		}
	}
	if found == nil {
		t.Fatalf("context %q not in %v", kctx, contexts)
	}

	conn, err := Load(fleet.KubeCluster{ID: "live-abc234", Context: kctx, KubeconfigPaths: paths, Namespace: "default"}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if conn.Server != found.Server || conn.Auth != found.Auth {
		t.Errorf("connection %s/%s, context listing %s/%s", conn.Server, conn.Auth, found.Server, found.Auth)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := cs.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	t.Logf("server %s %s, auth %s", conn.Server, v.GitVersion, conn.Auth)
	cred, err := conn.Credential(ctx)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	t.Logf("credential %+v", cred)
	if conn.Auth == AuthClientCert && !cred.ExpiresAt.After(time.Now()) {
		t.Errorf("client certificate expiry %v is not in the future", cred.ExpiresAt)
	}
}

// An exec on a real cluster runs past its connection's bound, on either
// protocol: the bound covers the dial and the upgrade only. It makes a
// busybox pod in a namespace of its own (fleet-it-p6-03-exec-*) and
// deletes it.
func TestLiveExecOutlivesItsConnectBound(t *testing.T) {
	kctx := os.Getenv(LiveContextEnv)
	if kctx == "" {
		t.Skip(LiveContextEnv + " is not set")
	}
	defer func(d time.Duration) { execConnectTimeout = d }(execConnectTimeout)
	execConnectTimeout = time.Second
	conn, err := Load(fleet.KubeCluster{ID: "live-abc234", Context: kctx, KubeconfigPaths: DefaultKubeconfigPaths(os.Environ()), Namespace: "default"}, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ns := fmt.Sprintf("fleet-it-p6-03-exec-%d", time.Now().UnixNano()%100000)
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) }()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sleeper"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main", Image: "busybox:1.37.0", ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sleep", "600"},
		}}},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, "sleeper", metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the pod did not run: %v", err)
		case <-time.After(time.Second):
		}
	}
	for _, protocol := range []string{ProtocolWebSocket, ProtocolSPDY} {
		var out strings.Builder
		start := time.Now()
		err := conn.Exec(ctx, ExecRequest{Namespace: ns, Pod: "sleeper", Container: "main", Command: []string{"sh", "-c", "sleep 3; echo outlived"}, Protocol: protocol},
			Stdio{Stdout: &out, Stderr: io.Discard})
		if err != nil || strings.TrimSpace(out.String()) != "outlived" {
			t.Fatalf("%s: exec = %v, output %q", protocol, err, out.String())
		}
		t.Logf("%s: a %s exec ran past its %s connect bound", protocol, time.Since(start).Round(time.Millisecond), execConnectTimeout)
	}
	// A stream (a pod agent session's transport) carries stdin past it too.
	st := conn.Transport().Stream(ctx, ExecRequest{Namespace: ns, Pod: "sleeper", Container: "main", Command: []string{"cat"}})
	defer st.Close()
	time.Sleep(2 * execConnectTimeout)
	if _, err := io.WriteString(st, "ping\n"); err != nil {
		t.Fatalf("stdin after the bound: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("the stream after the bound = %q, %v (stderr %q)", buf, err, st.Stderr())
	}
}
