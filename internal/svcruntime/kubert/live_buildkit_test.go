//go:build !nok8s && linux

package kubert

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestLiveBuildkitTLS runs a fleet's shared Buildkit on a real cluster
// (FLEET_KUBERT_LIVE=<context>, see live_test.go), rootless and then rootful,
// and builds on it from a pod of the fleet with buildctl: an instance's
// certificate builds, and its second build is cached; a 6-day-old one still
// builds; an expired one, one of another fleet's CA, and none at all fail
// the TLS handshake. The fleet leaving the connection rotates its CA.
func TestLiveBuildkitTLS(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBERT_LIVE")
	if kctx == "" {
		t.Skip("set FLEET_KUBERT_LIVE=<context> to run the Kubernetes runtime against a cluster")
	}
	orig := liveNamespacePrefix
	liveNamespacePrefix = "fleet-it-p5-02-buildkit-tcp-per-instance-"
	t.Cleanup(func() { liveNamespacePrefix = orig })
	r := newLiveRig(t, kctx)
	no := false
	r.rec.AllowPrivileged = &no
	r.saveRecord()
	fleetInstanceKeys = func(string, string) (map[string]string, error) { return map[string]string{"i1": "live-key-i1"}, nil }
	rt := For(r.rec.ID)
	m := r.meta("bk")
	ctx := context.Background()
	step := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			prev := r.t
			r.t = t
			defer func() { r.t = prev }()
			fn(t)
		})
	}
	if err := rt.PrepareFleet("bk"); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureService("bk", fleet.ServiceBuildkit); err != nil {
		t.Fatal(err)
	}
	client := r.buildkitClient(m)
	r.waitReady(m, fleet.ServiceBuildkit)
	host := m.name(names.SuffixBuildkit)

	// install writes cred into the client pod as a set of its own.
	install := func(set string, cred fleet.ServiceCredential) {
		r.t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "set -e; d=/tmp/%s; mkdir -p $d; ", set)
		for name, data := range map[string][]byte{"ca.pem": cred.CA, "cert.pem": cred.Cert, "key.pem": cred.Key} {
			fmt.Fprintf(&b, "printf %%s '%s' | base64 -d > $d/%s; ", base64.StdEncoding.EncodeToString(data), name)
		}
		if out, code := r.sh(client, "client", b.String()); code != 0 {
			r.t.Fatalf("installing %s: %s", set, out)
		}
	}
	// buildctl runs buildctl against the server with set's files (none:
	// "").
	buildctl := func(set, args string) (string, int) {
		tls := "--tlscacert /tmp/fresh/ca.pem --tlsservername " + host
		if set != "" {
			tls += fmt.Sprintf(" --tlscert /tmp/%s/cert.pem --tlskey /tmp/%s/key.pem", set, set)
		}
		return r.sh(client, "client", fmt.Sprintf("timeout 120 buildctl --addr tcp://%s:1234 %s %s 2>&1", host, tls, args))
	}
	// refused checks the server turns set's files away. The handshake's
	// end shows as the server's alert (tls: expired certificate, tls:
	// certificate required) or, when the client was still writing as the
	// server hung up, a broken pipe; the good certificates' builds just
	// before show the server is there.
	refused := func(t *testing.T, what, set string) {
		t.Helper()
		out, code := buildctl(set, "debug workers")
		if code == 0 || !strings.Contains(out, "failed to list workers") || strings.Contains(out, "connection refused") {
			t.Fatalf("%s (%d): %s", what, code, out)
		}
		t.Logf("%s: %s", what, firstLine(out))
	}
	build := "build --frontend dockerfile.v0 --local context=/tmp/ctx --local dockerfile=/tmp/ctx --progress plain"
	if out, code := r.sh(client, "client", `mkdir -p /tmp/ctx && printf 'FROM docker.io/library/busybox:1.37.0\nRUN echo fleet > /x && sleep 1\n' > /tmp/ctx/Dockerfile`); code != 0 {
		t.Fatal(out)
	}

	issueAt := func(at time.Time) fleet.ServiceCredential {
		t.Helper()
		orig := now
		now = func() time.Time { return at }
		defer func() { now = orig }()
		cred, err := rt.IssueCredential("bk", "i1", fleet.ServiceBuildkit)
		if err != nil {
			t.Fatal(err)
		}
		return cred
	}
	shapes := []struct {
		name    string
		rootful bool
	}{{"rootless", false}, {"rootful", true}}
	for _, shape := range shapes {
		step(shape.name, func(t *testing.T) {
			if shape.rootful {
				yes := true
				r.rec.AllowPrivileged = &yes
				r.saveRecord()
				if err := rt.EnsureService("bk", fleet.ServiceBuildkit); err != nil {
					t.Fatal(err)
				}
				r.waitRollout(m)
			}
			tpl := r.buildkitTemplate(m)
			if got := tpl.Spec.Containers[0].Image; (got == DefaultBuildkitImage) != shape.rootful {
				t.Fatalf("image %s", got)
			}
			install("fresh", issueAt(time.Now()))
			if out, code := buildctl("fresh", "debug workers"); code != 0 {
				t.Fatalf("a fresh certificate: %s", out)
			}
			first, code := buildctl("fresh", build)
			if code != 0 {
				t.Fatalf("build: %s", first)
			}
			out, code := buildctl("fresh", build)
			if code != 0 || !strings.Contains(out, "CACHED") {
				t.Fatalf("a second build is not cached (%d):\n%s\nthe first:\n%s", code, out, first)
			}
			install("old", issueAt(time.Now().Add(-6*24*time.Hour)))
			if out, code := buildctl("old", "debug workers"); code != 0 {
				t.Fatalf("a 6-day-old certificate: %s", out)
			}
			install("expired", issueAt(time.Now().Add(-7*24*time.Hour-time.Hour)))
			refused(t, "an expired certificate", "expired")
			other, _, err := ensureCA(r.rec.ID, r.meta("other"), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := other.issueClient("live-key-other", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			install("foreign", foreign)
			refused(t, "a certificate of another fleet's CA", "foreign")
			refused(t, "no certificate", "")
		})
	}

	step("CA rotation", func(t *testing.T) {
		fleetUsesCluster = func(string, string) bool { return false }
		defer func() { fleetUsesCluster = func(string, string) bool { return true } }()
		if err := rt.TeardownFleet("bk"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(state.KubeBuildkitCAPath(r.rec.ID, m.Hash)); !os.IsNotExist(err) {
			t.Fatalf("the CA outlived the fleet's teardown: %v", err)
		}
		_ = ctx
	})
}

// buildkitClient runs a pod of fleet m that has buildctl (the rootless
// Buildkit image, its server not started) and returns its name.
func (r *liveRig) buildkitClient(m fleetMeta) string {
	r.t.Helper()
	l := names.Base(m.Owner, m.Cluster)
	l[names.LabelFleet], l[names.LabelRole], l[names.LabelKey] = m.Hash, string(names.RoleInstance), "live-key-i1"
	zero := int64(0)
	uid := int64(1000)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildkit-client", Labels: l},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &zero,
			Containers: []corev1.Container{{
				Name: "client", Image: DefaultBuildkitRootlessImage, Command: []string{"sh", "-c", "exec sleep 3600"},
				SecurityContext: &corev1.SecurityContext{RunAsUser: &uid},
			}},
		},
	}
	if _, err := r.cs.CoreV1().Pods(r.ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		r.t.Fatal(err)
	}
	r.waitRunning(pod.Name)
	return pod.Name
}

func (r *liveRig) buildkitTemplate(m fleetMeta) corev1.PodTemplateSpec {
	r.t.Helper()
	d, err := r.cs.AppsV1().Deployments(r.ns).Get(context.Background(), m.name(names.SuffixBuildkit), metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return d.Spec.Template
}

// waitRollout waits for the Buildkit Deployment's new pod to be up.
func (r *liveRig) waitRollout(m fleetMeta) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := r.cs.AppsV1().Deployments(r.ns).Get(context.Background(), m.name(names.SuffixBuildkit), metav1.GetOptions{})
		if err == nil && d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas >= 1 && d.Status.AvailableReplicas >= 1 && d.Status.Replicas == 1 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	r.t.Fatal("the Buildkit server's new pod never came up")
}
