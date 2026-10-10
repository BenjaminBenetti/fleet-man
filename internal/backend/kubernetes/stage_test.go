//go:build !nok8s

package kubernetes

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
)

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// runStage runs stageScript in a real sh over a temp bin dir, the binary on
// stdin, and returns what it printed.
func runStage(t *testing.T, l PodLayout, data []byte, gzipped, checkSHA bool) (string, error) {
	t.Helper()
	wire := data
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
		wire = buf.Bytes()
	}
	script, err := stageScript(l, hexSHA(data), int64(len(data)), gzipped, int64(len(wire)), checkSHA)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = bytes.NewReader(wire)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String() + errOut.String(), err
	}
	return out.String(), nil
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStageScriptIsContentAddressed(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	l := PodLayout{InstallDir: root, BinDir: filepath.Join(root, "bin dir")} // a space, quoted
	v1, v2, v3 := []byte("fleet build one"), []byte("fleet build two"), []byte("fleet build three")

	out, err := runStage(t, l, v1, false, true)
	if err != nil || strings.TrimSpace(out) != "prev=" {
		t.Fatalf("first stage: %q %v", out, err)
	}
	if target, _ := os.Readlink(filepath.Join(l.BinDir, "fleet")); target != "fleet-"+hexSHA(v1) {
		t.Fatalf("link -> %q", target)
	}
	if got, _ := os.ReadFile(filepath.Join(l.BinDir, "fleet")); !bytes.Equal(got, v1) {
		t.Fatal("the link does not read the binary")
	}
	if fi, _ := os.Stat(filepath.Join(l.BinDir, "fleet-"+hexSHA(v1))); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}

	out, err = runStage(t, l, v2, true, false)
	if err != nil || strings.TrimSpace(out) != "prev=fleet-"+hexSHA(v1) {
		t.Fatalf("second stage (gzip): %q %v", out, err)
	}
	out, err = runStage(t, l, v3, false, true)
	if err != nil || strings.TrimSpace(out) != "prev=fleet-"+hexSHA(v2) {
		t.Fatalf("third stage: %q %v", out, err)
	}
	got := strings.Join(listDir(t, l.BinDir), " ")
	if want := "fleet fleet-" + hexSHA(v3) + " fleet-" + hexSHA(v2); len(listDir(t, l.BinDir)) != 3 || !strings.Contains(got, hexSHA(v3)) || !strings.Contains(got, hexSHA(v2)) {
		t.Fatalf("bin dir holds %q, want %q (the current and the previous build)", got, want)
	}

	// Staging the current build again changes nothing, the fallback included.
	out, err = runStage(t, l, v3, false, true)
	if err != nil || strings.TrimSpace(out) != "prev=" || len(listDir(t, l.BinDir)) != 3 {
		t.Fatalf("restaging the current build: %q %v, dir %v", out, err, listDir(t, l.BinDir))
	}

	// A stream that is not the binary leaves everything as it was.
	script, _ := stageScript(l, hexSHA([]byte("other")), 5, false, 5, true)
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader("wrong")
	if err := cmd.Run(); err == nil {
		t.Fatal("a stream with the wrong sha256 was staged")
	}
	if target, _ := os.Readlink(filepath.Join(l.BinDir, "fleet")); target != "fleet-"+hexSHA(v3) || len(listDir(t, l.BinDir)) != 3 {
		t.Fatalf("a failed stage changed the dir: %v (link %q)", listDir(t, l.BinDir), target)
	}
}

func TestProbeScriptReadsThePodsBuild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	l := PodLayout{InstallDir: root, BinDir: filepath.Join(root, "bin")}
	run := func() (jobboot.Probe, *podBuild) {
		t.Helper()
		out, err := exec.Command("sh", "-c", probeScript(l)).Output()
		if err != nil {
			t.Fatal(err)
		}
		probe, build, err := parseProbe(string(out))
		if err != nil {
			t.Fatalf("%v (%q)", err, out)
		}
		return probe, build
	}
	probe, build := run()
	if probe.Arch == "" || !probe.Head || build != nil {
		t.Fatalf("no binary: %+v %+v", probe, build)
	}
	_ = os.MkdirAll(l.BinDir, 0o755)
	stub := `#!/bin/sh
[ "$1 $2" = "pod-agent self-version" ] || exit 9
echo '{"version":"v1.2.3","sha":"abc","proto_min":1,"proto_max":2,"arch":"amd64","uid":0,"can_setuid":true}'
`
	if err := os.WriteFile(l.Binary(), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, build = run(); build == nil || build.Version != "v1.2.3" || build.SHA != "abc" || build.ProtoMin != 1 || build.ProtoMax != 2 {
		t.Fatalf("a binary: %+v", build)
	}
	// A binary that is not fleet (or does not run) is no build.
	_ = os.WriteFile(l.Binary(), []byte("#!/bin/sh\necho not json\n"), 0o755)
	if _, build = run(); build != nil {
		t.Fatalf("a foreign binary: %+v", build)
	}
	_ = os.WriteFile(l.Binary(), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if _, build = run(); build != nil {
		t.Fatalf("a failing binary: %+v", build)
	}
}

func TestStageReason(t *testing.T) {
	art := &binstore.Artifact{SHA256: "new"}
	cur := &podBuild{SHA: "new", ProtoMin: 1, ProtoMax: 1}
	old := &podBuild{SHA: "old", Version: "v1", ProtoMin: 1, ProtoMax: 1}
	future := &podBuild{SHA: "x", ProtoMin: 50, ProtoMax: 60}
	for _, tt := range []struct {
		name    string
		build   *podBuild
		art     *binstore.Artifact
		restage bool
		stage   bool
	}{
		{"missing", nil, art, false, true},
		{"missing, no build to stage", nil, nil, false, true},
		{"current", cur, art, true, false},
		{"older, not restaging", old, art, false, false},
		{"older, restaging", old, art, true, true},
		{"older, nothing to restage to", old, nil, true, false},
		{"another protocol", future, art, false, true},
	} {
		if got := stageReason(tt.build, tt.art, tt.restage); (got != "") != tt.stage {
			t.Errorf("%s: reason %q, want stage=%v", tt.name, got, tt.stage)
		}
	}
}

func TestPatchPID1Binary(t *testing.T) {
	spec := map[string]any{"format": 1, "argv": []string{"sh"}, "binary_sha256": "old", "some_future_field": map[string]int{"x": 1}}
	raw, _ := json.Marshal(spec)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fi-x-spec-g1", Namespace: "ns"}, Data: map[string][]byte{podspec.PID1File: raw, "exec.json": []byte("{}")}}
	cs := fake.NewClientset(sec)
	ctx := context.Background()
	if err := patchPID1Binary(ctx, cs, "ns", "fi-x-spec-g1", "new", "old"); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.CoreV1().Secrets("ns").Get(ctx, "fi-x-spec-g1", metav1.GetOptions{})
	var after map[string]any
	if err := json.Unmarshal(got.Data[podspec.PID1File], &after); err != nil {
		t.Fatal(err)
	}
	if after["binary_sha256"] != "new" || after["fallback_sha256"] != "old" || after["some_future_field"] == nil || after["format"] != float64(1) {
		t.Fatalf("pid1.json after the patch: %v", after)
	}
	if string(got.Data["exec.json"]) != "{}" {
		t.Fatal("the patch changed another spec file")
	}
	// Patching to what it already says is a no-op (no update).
	cs.ClearActions()
	if err := patchPID1Binary(ctx, cs, "ns", "fi-x-spec-g1", "new", "old"); err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" {
			t.Fatal("an unchanged pid1.json was written")
		}
	}
	if err := patchPID1Binary(ctx, cs, "ns", "missing", "new", "old"); err == nil {
		t.Fatal("a missing Secret patched")
	}
}
