//go:build linux

package podseed_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	. "github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// TestKubeSeedAndPodInit runs the whole persistent-root path on a real
// cluster with the static pod binary: a seed Job bootstrapped by the jobboot
// protocol (probe, gzip-staged binary, spec, a workspace fed over a kube exec
// of `fleet pod-job feed`), then a StatefulSet whose PID 1 is `fleet pod-init`
// over the seeded PVC (privileged, so the cgroup layout is real), then a pod
// restart, a rebuild reseed with reset and a rollback Job. It needs a cluster,
// so it runs only when FLEET_PODSEED_KUBE_CONTEXT names a kube context; it
// creates a namespace named fleet-it-p2-03-seed-* and deletes it when done.
// FLEET_PODSEED_KUBE_IMAGE picks the image (default: the devcontainers base
// image, which has a vscode user, sudo and usrmerge symlinks).
func TestKubeSeedAndPodInit(t *testing.T) {
	kctx := os.Getenv("FLEET_PODSEED_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_PODSEED_KUBE_CONTEXT=<context> to seed and boot a real pod")
	}
	image := os.Getenv("FLEET_PODSEED_KUBE_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	k := &kube{t: t, context: kctx, ns: "fleet-it-p2-03-seed-" + hex.EncodeToString(suffix[:]), image: image}
	k.bin, k.binSHA = buildPodBinary(t)
	k.must(nil, "create", "namespace", k.ns)
	t.Cleanup(func() { _, _, _ = k.run(nil, "delete", "namespace", k.ns, "--wait=false") })

	const sts = "fi-seedtest"
	k.apply(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": sts},
		"spec": map[string]any{"accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "5Gi"}}}})

	seed := JobSpec{V: JobSpecVersion, Op: OpSeed, Gen: 1, BinarySHA256: k.binSHA, PVC: "/fleet-pvc",
		Seed: &SeedSpec{
			MountDirs: []string{"/workspaces/seedtest", "/opt/fleet", "/var/lib/docker", "/fleet-mounts/control"},
			Hostname:  sts + "-0",
			Remap:     &Remap{User: "vscode", UID: 4321, GID: 4321},
			Volumes:   []Volume{{Name: "docker", Target: "/var/lib/docker", CopyUp: true}},
			Workspace: &Workspace{Owner: &IDs{UID: 4321, GID: 4321}},
		}}
	ws := tarOf(t, map[string]string{"README.md": "# seedtest\n", "src/main.go": "package main\n"})
	start := time.Now()
	res, term := k.runJob(sts+"-seed-g1", sts, seed, ws)
	t.Logf("seed g1 in %s", time.Since(start).Round(time.Second))
	m, err := ParseManifest(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("manifest: dirs %v root-extra %v files %d bytes %d setuid %d caps %d warnings %v",
		m.Dirs, m.RootExtra, m.Files, m.Bytes, m.Setuid, m.Capabilities, m.Warnings)
	if !term.OK || term.SHA256 != sha256Hex(res.Output) || term.Bytes < m.Bytes {
		t.Fatalf("termination %+v vs the result's manifest", term)
	}
	if !contains(m.Dirs, "etc") || !contains(m.Dirs, "usr") || contains(m.Dirs, "bin") || contains(m.Dirs, "proc") ||
		!contains(m.RootExtra, "workspaces") || m.Setuid < 5 || m.Remap == nil || !m.Remap.Changed || m.SingleUID {
		t.Fatalf("manifest %+v", m)
	}

	k.applyInstance(sts, m, 1)
	pod := sts + "-0"
	k.waitReady(pod)
	sh := func(script string) string {
		return strings.TrimSpace(k.must(nil, "exec", "-n", k.ns, pod, "-c", "dev", "--", "sh", "-c", script))
	}

	t.Run("pod-init is PID 1 and laid out the cgroups", func(t *testing.T) {
		if got := sh(`tr '\0' ' ' < /proc/1/cmdline`); !strings.Contains(got, "pod-init --spec /opt/fleet/spec/pid1.json") {
			t.Fatalf("PID 1 is %q", got)
		}
		pid1 := sh(`sed -n 's/^0:://p' /proc/1/cgroup`)
		if !strings.HasSuffix(pid1, "/fleet-agent") {
			t.Fatalf("PID 1's cgroup %q", pid1)
		}
		c := "/sys/fs/cgroup" + strings.TrimSuffix(pid1, "/fleet-agent")
		if got := sh("cat " + c + "/memory.oom.group"); got != "0" {
			t.Errorf("memory.oom.group %q", got)
		}
		if got := sh("cat " + c + "/fleet-user/memory.max"); got != strconv.Itoa(1<<30-64<<20) {
			t.Errorf("fleet-user memory.max %q", got)
		}
		script := sh(`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = sleep ] && sed -n 's/^0:://p' $p/cgroup && break; done`)
		if !strings.HasSuffix(script, "/fleet-user") {
			t.Errorf("the start script's sleep runs in %q, want fleet-user", script)
		}
		if layout := sh("cat /opt/fleet/run/cgroup.json"); !strings.Contains(layout, `"oom_group":true`) {
			t.Errorf("cgroup.json %s", layout)
		}
	})
	t.Run("the root generation works as the image did", func(t *testing.T) {
		for script, want := range map[string]string{
			"readlink /usr/bin/fleet":                                    "/opt/fleet/bin/fleet",
			"/usr/bin/fleet version >/dev/null && echo ok":               "ok",
			"id -u vscode; stat -c %u /home/vscode":                      "4321\n4321",
			"su vscode -c 'sudo -n id -u'":                               "0",
			"stat -c '%a %U' /usr/bin/sudo":                              "4755 root",
			"cat /workspaces/seedtest/README.md":                         "# seedtest",
			"stat -c %u /workspaces/seedtest/src/main.go":                "4321",
			"hostname; cat /etc/hostname":                                pod + "\n" + pod,
			"getent hosts kubernetes.default.svc >/dev/null && echo dns": "dns",
			"ls -ld /bin | sed 's/.*-> //'":                              "usr/bin",
			"grep -c 'Container started' /opt/fleet/log/pid1.log":        "1",
		} {
			if got := sh(script); got != want {
				t.Errorf("%s: %q, want %q", script, got, want)
			}
		}
	})

	// What the user changes persists across a pod restart.
	sh(`echo kept > /usr/local/bin/marker && echo kept > /etc/marker && mkdir -p /workspaces/sibling && echo kept > /workspaces/sibling/x && su vscode -c 'echo kept > ~/marker'`)
	oldUID := k.must(nil, "get", "pod", "-n", k.ns, pod, "-o", "jsonpath={.metadata.uid}")
	k.must(nil, "delete", "pod", "-n", k.ns, pod, "--wait=true")
	k.waitNewPod(pod, oldUID)
	t.Run("a restart keeps the root", func(t *testing.T) {
		if got := sh("cat /usr/local/bin/marker /etc/marker /workspaces/sibling/x /home/vscode/marker"); got != "kept\nkept\nkept\nkept" {
			t.Fatalf("after a restart: %q", got)
		}
		if got := sh("grep -c 'Container started' /opt/fleet/log/pid1.log"); got != "2" {
			t.Errorf("the rolling file holds %s runs' output, want 2", got)
		}
	})

	// A rebuild reseeds with reset; a failure later rolls it back.
	k.must(nil, "scale", "statefulset", "-n", k.ns, sts, "--replicas=0")
	k.must(nil, "wait", "-n", k.ns, "--for=delete", "pod/"+pod, "--timeout=120s")
	reseed := seed
	reseedSpec := *seed.Seed
	reseedSpec.Reset, reseedSpec.PrevGen, reseedSpec.Workspace = true, 1, nil
	reseed.Gen, reseed.Seed = 2, &reseedSpec
	res, _ = k.runJob(sts+"-seed-g2", sts, reseed, nil)
	if m2, _ := ParseManifest(res.Output); m2.Gen != 2 {
		t.Fatalf("reseed manifest %+v", m2)
	}
	res, _ = k.runJob(sts+"-tool-rollback", sts, JobSpec{V: JobSpecVersion, Op: OpRollback, Gen: 1, BinarySHA256: k.binSHA, PVC: "/fleet-pvc"}, nil)
	if m1, _ := ParseManifest(res.Output); m1.Gen != 1 {
		t.Fatalf("rollback manifest %+v", m1)
	}
	k.must(nil, "scale", "statefulset", "-n", k.ns, sts, "--replicas=1")
	k.waitReady(pod)
	t.Run("a rollback restores generation 1", func(t *testing.T) {
		if got := sh("cat /etc/marker /workspaces/sibling/x"); got != "kept\nkept" {
			t.Fatalf("after the rollback: %q", got)
		}
	})
}

// TestKubeSeedAndPodInitTierC is the same path in a namespace that enforces
// the restricted Pod Security level, every pod running as an arbitrary
// non-root uid (capability tier C): the seed copies as that uid and keeps what
// it can read, the remap is skipped, and pod-init appends the uid to the
// copied /etc/passwd, lays out no cgroups and runs the start script as it.
func TestKubeSeedAndPodInitTierC(t *testing.T) {
	kctx := os.Getenv("FLEET_PODSEED_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_PODSEED_KUBE_CONTEXT=<context> to seed and boot a real pod")
	}
	image := os.Getenv("FLEET_PODSEED_KUBE_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	k := &kube{t: t, context: kctx, ns: "fleet-it-p2-03-seed-rs-" + hex.EncodeToString(suffix[:]), image: image, tierC: true}
	k.bin, k.binSHA = buildPodBinary(t)
	k.must(nil, "create", "namespace", k.ns)
	t.Cleanup(func() { _, _, _ = k.run(nil, "delete", "namespace", k.ns, "--wait=false") })
	k.must(nil, "label", "namespace", k.ns, "pod-security.kubernetes.io/enforce=restricted")

	const sts = "fi-tierc"
	k.apply(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": sts},
		"spec": map[string]any{"accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "5Gi"}}}})
	seed := JobSpec{V: JobSpecVersion, Op: OpSeed, Gen: 1, BinarySHA256: k.binSHA, PVC: "/fleet-pvc",
		Seed: &SeedSpec{
			MountDirs: []string{"/workspaces/seedtest", "/opt/fleet", "/var/lib/docker", "/fleet-mounts/control"},
			Hostname:  sts + "-0",
			Remap:     &Remap{User: "vscode", UID: 4321, GID: 4321},
			Volumes:   []Volume{{Name: "docker", Target: "/var/lib/docker", CopyUp: true}},
			Workspace: &Workspace{Owner: &IDs{UID: 4321, GID: 4321}},
		}}
	res, term := k.runJob(sts+"-seed-g1", sts, seed, tarOf(t, map[string]string{"README.md": "# tier c\n"}))
	m, err := ParseManifest(res.Output)
	if err != nil || !term.OK {
		t.Fatalf("%+v %v %+v", m, err, term)
	}
	t.Logf("manifest: dirs %v files %d warnings %d (first %v)", m.Dirs, m.Files, len(m.Warnings), first(m.Warnings, 3))
	if !m.SingleUID || m.Remap != nil || !strings.Contains(strings.Join(m.Warnings, "\n"), "no uid remap without root") {
		t.Fatalf("a tier C seed: single_uid %v remap %+v", m.SingleUID, m.Remap)
	}

	k.applyInstance(sts, m, 1)
	pod := sts + "-0"
	k.waitReady(pod)
	sh := func(script string) string {
		return strings.TrimSpace(k.must(nil, "exec", "-n", k.ns, pod, "-c", "dev", "--", "sh", "-c", script))
	}
	for script, want := range map[string]string{
		"id -u; id -un":                      strconv.Itoa(tierCUID) + "\nfleet",
		"tail -n 1 /etc/passwd":              fmt.Sprintf("fleet:x:%d:0:fleet pod user:/home/vscode:/bin/bash", tierCUID),
		"readlink /usr/bin/fleet":            "/opt/fleet/bin/fleet",
		"cat /workspaces/seedtest/README.md": "# tier c",
		"test -e /opt/fleet/run/cgroup.json && echo layout || echo none":                                                                         "none",
		"s=$(for p in /proc/[0-9]*; do [ \"$(cat $p/comm 2>/dev/null)\" = sleep ] && awk '/^Uid:/{print $2}' $p/status && break; done); echo $s": strconv.Itoa(tierCUID),
		"echo kept > /usr/local/marker 2>&1 && cat /usr/local/marker":                                                                            "kept",
	} {
		if got := sh(script); got != want {
			t.Errorf("%s: %q, want %q", script, got, want)
		}
	}
	oldUID := k.must(nil, "get", "pod", "-n", k.ns, pod, "-o", "jsonpath={.metadata.uid}")
	k.must(nil, "delete", "pod", "-n", k.ns, pod, "--wait=true")
	k.waitNewPod(pod, oldUID)
	if got := sh("cat /usr/local/marker; grep -c '^fleet:' /etc/passwd"); got != "kept\n1" {
		t.Errorf("after a restart: %q (the marker, and one appended entry, not two)", got)
	}
}

func first(list []string, n int) []string {
	return list[:min(n, len(list))]
}

// kube runs kubectl against one context and namespace.
type kube struct {
	t                  *testing.T
	context, ns, image string
	bin, binSHA        string
	// tierC runs every pod as an arbitrary non-root uid, as a restricted
	// namespace (or OpenShift's restricted-v2) requires.
	tierC bool
}

// tierCUID is the arbitrary uid a tier C pod runs as.
const tierCUID = 1000650000

// security is the pod-level and container-level securityContext of a Job
// (privileged false) or an instance pod.
func (k *kube) security(privileged bool) (pod, container map[string]any) {
	if !k.tierC {
		container = map[string]any{"runAsUser": 0}
		if privileged {
			container["privileged"] = true
		}
		return map[string]any{}, container
	}
	return map[string]any{"runAsNonRoot": true, "runAsUser": tierCUID, "runAsGroup": 0, "fsGroup": tierCUID,
			"fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}}
}

func (k *kube) run(stdin []byte, args ...string) (string, string, int) {
	cmd := exec.Command("kubectl", append([]string{"--context", k.context}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	_ = cmd.Run()
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

func (k *kube) must(stdin []byte, args ...string) string {
	k.t.Helper()
	out, errOut, code := k.run(stdin, args...)
	if code != 0 {
		k.t.Fatalf("kubectl %s: exit %d: %s%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func (k *kube) apply(obj map[string]any) {
	k.t.Helper()
	data, _ := json.Marshal(obj)
	k.must(data, "apply", "-n", k.ns, "-f", "-")
}

// runJob runs one jobboot Job on the PVC the way fleetd will: create it, wait
// for its pod, probe, stage the binary (gzip when the image has it) and the
// spec, feed it over `kubectl exec -i ... pod-job feed`, then read its
// termination message.
func (k *kube) runJob(name, pvc string, spec JobSpec, workspace []byte) (jobboot.Result, jobboot.Termination) {
	k.t.Helper()
	paths := jobboot.Paths{}
	podSec, ctrSec := k.security(false)
	k.apply(map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": 900, "ttlSecondsAfterFinished": 600,
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"fleet-test-job": name}},
				"spec": map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
					"securityContext": podSec,
					"containers": []any{map[string]any{"name": "job", "image": k.image, "imagePullPolicy": "IfNotPresent",
						"command": jobboot.Command(paths, 600), "securityContext": ctrSec,
						"volumeMounts": []any{map[string]any{"name": "pvc", "mountPath": spec.PVC}, map[string]any{"name": "tools", "mountPath": jobboot.ToolsDir}}}},
					"volumes": []any{map[string]any{"name": "pvc", "persistentVolumeClaim": map[string]any{"claimName": pvc}},
						map[string]any{"name": "tools", "emptyDir": map[string]any{}}}}}}})
	k.must(nil, "wait", "-n", k.ns, "--for=condition=Ready", "pod", "-l", "fleet-test-job="+name, "--timeout=300s")
	pod := strings.TrimSpace(k.must(nil, "get", "pod", "-n", k.ns, "-l", "fleet-test-job="+name, "-o", "jsonpath={.items[0].metadata.name}"))

	probe, err := jobboot.ParseProbe(k.must(nil, "exec", "-n", k.ns, pod, "--", "sh", "-c", jobboot.ProbeScript))
	wantUID := 0
	if k.tierC {
		wantUID = tierCUID
	}
	if err != nil || probe.Arch != runtime.GOARCH || probe.UID != wantUID || !probe.Head {
		k.t.Fatalf("probe %+v %v", probe, err)
	}
	bin, _ := os.ReadFile(k.bin)
	wire := bin
	f := jobboot.StageFile{Path: paths.Binary(), Size: int64(len(bin)), SHA256: k.binSHA, Mode: 0o755, CheckSHA: probe.SHA256Sum}
	if probe.Gzip {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(bin)
		_ = zw.Close()
		wire, f.Gzip, f.WireSize = gz.Bytes(), true, int64(gz.Len())
	}
	script, err := jobboot.StageScript(f)
	if err != nil {
		k.t.Fatal(err)
	}
	k.must(wire, "exec", "-i", "-n", k.ns, pod, "--", "sh", "-c", script)
	specData, _ := json.Marshal(spec)
	script, _ = jobboot.StageScript(jobboot.StageFile{Path: paths.Spec(), Size: int64(len(specData)), SHA256: sha256Hex(specData), Ready: true, CheckSHA: probe.SHA256Sum})
	k.must(specData, "exec", "-i", "-n", k.ns, pod, "--", "sh", "-c", script)

	cmd := exec.Command("kubectl", "--context", k.context, "exec", "-i", "-n", k.ns, pod, "--", paths.Binary(), "pod-job", "feed")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var feedErr bytes.Buffer
	cmd.Stderr = &feedErr
	if err := cmd.Start(); err != nil {
		k.t.Fatal(err)
	}
	opts := jobboot.FeedOptions{Progress: func(p jobboot.Progress) {
		if p.Done {
			k.t.Logf("%s: %s %s done (%d files, %d bytes)", name, p.Phase, p.Detail, p.Files, p.Bytes)
		}
	}}
	if workspace != nil {
		opts.Workspace = bytes.NewReader(workspace)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := jobboot.Feed(ctx, struct {
		io.Reader
		io.Writer
	}{stdout, stdin}, opts)
	_ = stdin.Close()
	_ = cmd.Wait()
	if err != nil {
		logs, _, _ := k.run(nil, "logs", "-n", k.ns, pod)
		k.t.Fatalf("feed %s: %v\nfeed stderr: %s\njob log:\n%s", name, err, feedErr.String(), logs)
	}
	if !res.OK {
		logs, _, _ := k.run(nil, "logs", "-n", k.ns, pod)
		k.t.Fatalf("%s failed: %s\n%s", name, res.Error, logs)
	}
	k.must(nil, "wait", "-n", k.ns, "--for=condition=complete", "job/"+name, "--timeout=120s")
	msg := k.must(nil, "get", "pod", "-n", k.ns, pod, "-o", "jsonpath={.status.containerStatuses[0].state.terminated.message}")
	term, err := jobboot.ParseTermination(msg)
	if err != nil {
		k.t.Fatalf("termination message %q: %v", msg, err)
	}
	return res, term
}

// applyInstance creates the instance StatefulSet over the seeded PVC: every
// persisted dir by subPath, the kubelet-file overrides from meta/, the spec
// Secret, and PID 1 = fleet pod-init.
func (k *kube) applyInstance(sts string, m Manifest, replicas int) {
	k.t.Helper()
	pid1 := podinit.Spec{Format: podinit.SpecVersion, RemoteUser: "vscode", BinarySHA256: m.Binary,
		Argv: []string{"/bin/sh", "-c", "echo Container started\ntrap \"exit 0\" 15\nexec \"$@\"\nwhile sleep 1 & wait $!; do :; done", "-"}}
	pid1Data, _ := json.Marshal(pid1)
	k.apply(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": sts + "-spec"},
		"stringData": map[string]any{"pid1.json": string(pid1Data)}})
	k.apply(map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": sts + "-net"},
		"spec": map[string]any{"clusterIP": "None", "selector": map[string]any{"app": sts}}})

	podSec, ctrSec := k.security(true)
	var mounts []any
	for _, d := range m.Dirs {
		mounts = append(mounts, map[string]any{"name": "disk", "mountPath": "/" + d, "subPath": RootSubPath(d)})
	}
	for _, d := range m.RootExtra {
		mounts = append(mounts, map[string]any{"name": "disk", "mountPath": "/" + d, "subPath": RootExtraSubPath(d)})
	}
	mounts = append(mounts,
		map[string]any{"name": "disk", "mountPath": "/workspaces/seedtest", "subPath": DirWorkspace},
		map[string]any{"name": "disk", "mountPath": "/var/lib/docker", "subPath": VolumeSubPath("docker")},
		map[string]any{"name": "disk", "mountPath": InstallDir, "subPath": DirFleet},
		map[string]any{"name": "disk", "mountPath": "/etc/resolv.conf", "subPath": ResolvConfFile},
		map[string]any{"name": "disk", "mountPath": "/etc/hostname", "subPath": HostnameFile},
		map[string]any{"name": "spec", "mountPath": SpecDir, "readOnly": true},
		map[string]any{"name": "run", "mountPath": RunDir},
		map[string]any{"name": "control", "mountPath": "/fleet-mounts/control"})
	k.apply(map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": sts},
		"spec": map[string]any{"replicas": replicas, "serviceName": sts + "-net", "podManagementPolicy": "Parallel",
			"updateStrategy": map[string]any{"type": "OnDelete"},
			"selector":       map[string]any{"matchLabels": map[string]any{"app": sts}},
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": sts}},
				"spec": map[string]any{"automountServiceAccountToken": false, "enableServiceLinks": false, "terminationGracePeriodSeconds": 10,
					"securityContext": podSec,
					"containers": []any{map[string]any{"name": "dev", "image": k.image, "imagePullPolicy": "IfNotPresent",
						"command":         []string{CurrentBinary, "pod-init", "--spec", podinit.DefaultSpecPath},
						"securityContext": ctrSec,
						"resources":       map[string]any{"requests": map[string]any{"memory": "256Mi"}, "limits": map[string]any{"memory": "1Gi"}},
						"env":             []any{map[string]any{"name": podinit.PodUIDEnv, "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}},
						"volumeMounts":    mounts}},
					"volumes": []any{
						map[string]any{"name": "disk", "persistentVolumeClaim": map[string]any{"claimName": sts}},
						map[string]any{"name": "spec", "secret": map[string]any{"secretName": sts + "-spec", "defaultMode": 0o400}},
						map[string]any{"name": "run", "emptyDir": map[string]any{}},
						map[string]any{"name": "control", "emptyDir": map[string]any{"medium": "Memory"}}}}}}})
}

func (k *kube) waitReady(pod string) {
	k.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, _, code := k.run(nil, "wait", "-n", k.ns, "--for=condition=Ready", "pod/"+pod, "--timeout=10s"); code == 0 {
			return
		}
		if time.Now().After(deadline) {
			desc, _, _ := k.run(nil, "describe", "pod", "-n", k.ns, pod)
			logs, _, _ := k.run(nil, "logs", "-n", k.ns, pod)
			k.t.Fatalf("%s never became Ready\n%s\n%s", pod, desc, logs)
		}
	}
}

// waitNewPod waits for the StatefulSet to replace pod (a new uid) and the new
// one to be Ready.
func (k *kube) waitNewPod(pod, oldUID string) {
	k.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		uid, _, code := k.run(nil, "get", "pod", "-n", k.ns, pod, "-o", "jsonpath={.metadata.uid}")
		if code == 0 && uid != "" && uid != oldUID {
			break
		}
		if time.Now().After(deadline) {
			k.t.Fatalf("%s was not recreated", pod)
		}
		time.Sleep(time.Second)
	}
	k.waitReady(pod)
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()})
	dirs := map[string]bool{}
	for name, body := range files {
		if d := filepath.Dir(name); d != "." && !dirs[d] {
			dirs[d] = true
			_ = tw.WriteHeader(&tar.Header{Name: "./" + d + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()})
		}
		_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.Bytes()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// buildPodBinary builds the static pod flavor of fleet (-tags nok8s) for the
// cluster's arch (FLEET_PODSEED_KUBE_ARCH, default this machine's).
func buildPodBinary(t *testing.T) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fleet")
	arch := os.Getenv("FLEET_PODSEED_KUBE_ARCH")
	if arch == "" {
		arch = runtime.GOARCH
	}
	cmd := exec.Command("go", "build", "-tags", "nok8s", "-o", out, "github.com/BenjaminBenetti/fleet-man/cmd/fleet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the pod binary: %v\n%s", err, msg)
	}
	sha, err := FileSHA256(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, sha
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
